// Package sessionindex maintains a token-prefix inverted index over the
// user and assistant text of persisted zut sessions.
package sessionindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// Mode selects how query tokens must co-occur.
type Mode int

const (
	// ModeSessions requires every token somewhere in the same session.
	ModeSessions Mode = iota
	// ModeMessages requires every token in the same message.
	ModeMessages
)

// Options configures an Index.
type Options struct {
	// Skip reports messages that must not be indexed (hidden messages).
	Skip func(provider.Message) bool
}

// Query is a parsed search request.
type Query struct {
	Text string
	Mode Mode
	// CWD restricts results to sessions of this cwd; set for @here.
	CWD   string
	Limit int
}

// SessionDoc describes one indexed session file.
type SessionDoc struct {
	Path, ID, CWD, Title string
	Started, Updated     time.Time
}

// HitRef locates one matching message row in a session file.
type HitRef struct {
	Path        string
	MsgSeq      int
	TotalMsgs   int
	Fingerprint uint64
	Role        provider.Role
	Time        time.Time
	// Offset and Len are the byte range of the JSONL row.
	Offset, Len int64
}

// SessionMatch groups hits of one session.
type SessionMatch struct {
	Doc   SessionDoc
	Hits  []HitRef
	Count int
}

// Result is the answer to a Query.
type Result struct {
	Sessions []SessionMatch
	// Partial is set when a token's prefix expansion was capped, or when no
	// index exists yet.
	Partial bool
}

// Snippet is the text of one hit, read back from its session file.
type Snippet struct {
	Ref  HitRef
	Text string
}

// Index is an on-disk session text index stored in <root>/search. Open is
// cheap; the segment is opened lazily by Query and reopened when another
// writer publishes a new generation. An Index is safe for concurrent use.
type Index struct {
	root string
	dir  string
	opts Options

	mu     sync.Mutex
	seg    *segment
	segGen uint64
}

// Open returns the index for the zut state root (the directory holding
// sessions/). It does not touch the disk.
func Open(root string, opts Options) (*Index, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("sessionindex: empty root")
	}
	return &Index{root: root, dir: filepath.Join(root, searchDirName), opts: opts}, nil
}

// Close releases the open segment. The writer lock is only held during
// Update and Forget.
func (x *Index) Close() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	var err error
	if x.seg != nil {
		err = x.seg.Close()
		x.seg, x.segGen = nil, 0
	}
	return err
}

// Update indexes new, appended, and rewritten sessions and drops deleted
// ones across every project, then publishes a new generation. Nothing is
// written when no session changed. A missing or corrupt index is rebuilt.
// progress reports scanned files; it is called from worker goroutines,
// serialized. When another writer holds the lock it returns ErrBusy without
// writing; queries keep using the current index. An unreadable session
// directory aborts the update without changing the index.
func (x *Index) Update(ctx context.Context, progress func(done, total int)) error {
	_, err := x.update(ctx, progress)
	return err
}

// Query runs q against the current generation. A query without tokens
// (empty, or only "@here") returns the Recent sessions with no hits. Without an index yet it
// returns an empty Partial result. A corrupt index returns an error wrapping
// ErrCorrupt; Update rebuilds it.
func (x *Index) Query(ctx context.Context, q Query) (Result, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	seg, err := x.currentLocked()
	if err != nil {
		return Result{}, err
	}
	if seg == nil {
		return Result{Partial: true}, nil
	}
	return runQuery(ctx, seg, q)
}

// currentLocked returns the live segment, reopening it when the published
// generation changed. It returns nil when no index exists.
func (x *Index) currentLocked() (*segment, error) {
	gen, err := readCurrent(x.dir)
	if err != nil {
		return nil, err
	}
	if gen == x.segGen && x.seg != nil {
		return x.seg, nil
	}
	if x.seg != nil {
		x.seg.Close()
		x.seg, x.segGen = nil, 0
	}
	if gen == 0 {
		return nil, nil
	}
	seg, err := openSegment(filepath.Join(x.dir, segName(gen)))
	if errors.Is(err, os.ErrNotExist) {
		// A writer published and cleaned up between the two reads: retry once.
		if gen, err = readCurrent(x.dir); err == nil && gen != 0 {
			seg, err = openSegment(filepath.Join(x.dir, segName(gen)))
		}
	}
	if err != nil || gen == 0 {
		return nil, err
	}
	x.seg, x.segGen = seg, gen
	return seg, nil
}

// Recent returns indexed sessions newest first (Updated, then path),
// filtered by cwd when non-empty and capped by limit when positive. It reads
// no postings or rows. Without an index it returns nil.
func (x *Index) Recent(ctx context.Context, cwd string, limit int) ([]SessionDoc, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	seg, err := x.currentLocked()
	if err != nil || seg == nil {
		return nil, err
	}
	return recentDocs(seg.docs, cwd, limit), nil
}

// maxSnippetRunes caps the text returned per snippet.
const maxSnippetRunes = 4096

// Snippets reads the text of refs back from their session files, in refs
// order. A ref is skipped when its session is no longer indexed, the file
// was replaced or truncated, or the row no longer has ref's fingerprint.
// Texts are joined with a space and capped at maxSnippetRunes; they are not
// sanitized.
func (x *Index) Snippets(ctx context.Context, refs []HitRef) ([]Snippet, error) {
	x.mu.Lock()
	seg, err := x.currentLocked()
	heads := map[string]uint64{}
	if err == nil && seg != nil {
		for _, r := range refs {
			heads[r.Path] = 0
		}
		for _, d := range seg.docs {
			if _, ok := heads[d.Path]; ok && !d.Excluded {
				heads[d.Path] = d.Cursor.HeadHash
			}
		}
	}
	x.mu.Unlock()
	if err != nil || seg == nil {
		return nil, err
	}

	byPath := map[string][]int{}
	var order []string
	for i, r := range refs {
		if _, ok := byPath[r.Path]; !ok {
			order = append(order, r.Path)
		}
		byPath[r.Path] = append(byPath[r.Path], i)
	}
	texts := make([]*string, len(refs))
	for _, path := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if head := heads[path]; head != 0 && x.managedPath(path) {
			readSnippets(path, head, refs, byPath[path], texts)
		}
	}
	var out []Snippet
	for i, t := range texts {
		if t != nil {
			out = append(out, Snippet{Ref: refs[i], Text: *t})
		}
	}
	return out, nil
}

// managedPath reports whether path is a regular, non-symlink *.jsonl file
// directly inside a bucket of <root>/sessions, so Snippets never opens a
// path from a damaged or crafted index elsewhere.
func (x *Index) managedPath(path string) bool {
	clean := filepath.Clean(path)
	if filepath.Ext(clean) != ".jsonl" || filepath.Dir(filepath.Dir(clean)) != filepath.Clean(filepath.Join(x.root, "sessions")) {
		return false
	}
	info, err := os.Lstat(clean)
	return err == nil && info.Mode().IsRegular()
}

// readSnippets fills texts[i] for the refs of one file that still match.
func readSnippets(path string, head uint64, refs []HitRef, idx []int, texts []*string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	if h, ok, err := core.SessionHeadHash(f); err != nil || !ok || h != head {
		return
	}
	for _, i := range idx {
		ref := refs[i]
		if ref.Offset+ref.Len > info.Size() {
			continue
		}
		row, err := core.ReadSessionTextRowAt(f, ref.Offset, ref.Len)
		if err != nil || row.Fingerprint != ref.Fingerprint {
			continue
		}
		text := capRunes(strings.Join(row.Texts, " "), maxSnippetRunes)
		texts[i] = &text
	}
}

func capRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// Forget removes path and its text from the index now by publishing a new
// generation. It returns ErrBusy when another writer holds the lock; the
// next Update also drops sessions whose file is gone.
func (x *Index) Forget(path string) error {
	_, err := x.forget(path)
	return err
}
