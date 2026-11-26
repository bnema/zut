package sessionindex

import (
	"context"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// maxPrefixTerms caps how many dictionary terms one query token expands to.
// A capped expansion marks the result Partial.
const maxPrefixTerms = 4096

// docEntry is one indexed session. Fields map 1:1 to the on-disk doc record.
type docEntry struct {
	SessionDoc
	// Size is the file size at index time; Updated is its mtime.
	Size   int64
	Cursor core.SessionScanCursor
	// TotalMsgs is the number of message rows (Cursor.MsgSeq). Global
	// message IDs of this doc are first[d] .. first[d]+TotalMsgs-1.
	TotalMsgs int
	// Excluded marks a file that is not listable (hidden or invalid). It is
	// kept with only Path, Size, and Updated so unchanged files are not
	// re-checked; it has no rows or postings.
	Excluded bool
}

// rowEntry locates one indexed message row. It is fixed width on disk.
type rowEntry struct {
	MsgID       uint32
	Offset      int64
	Len         int64
	Role        provider.Role
	Time        time.Time
	Fingerprint uint64
}

// memIndex is a built index generation in memory. encodeSegment writes it
// section by section.
type memIndex struct {
	docs []docEntry
	// first[d] is the first global message ID of docs[d]; len(docs)+1 entries.
	first []uint32
	// terms is sorted; postings[i] lists ascending global message IDs.
	terms    []string
	postings [][]uint32
	// rows holds indexed rows sorted by MsgID; rowFirst[d] is the first row
	// of docs[d]; len(docs)+1 entries.
	rows     []rowEntry
	rowFirst []uint32
}

// docBuild is the per-file extraction result before global numbering.
type docBuild struct {
	doc   docEntry
	terms map[string][]uint32 // term -> ascending local MsgSeq
	rows  []rowEntry          // MsgID holds the local MsgSeq
	skip  bool                // hidden or unreadable: not indexed
	// reset reports that a resumed scan restarted at 0 (file rewritten).
	reset bool
	// scanned counts the session bytes read for this doc across every pass
	// (listability check, summary, text scan).
	scanned int64
}

func buildWorkers() int { return max(1, runtime.NumCPU()/2) }

// scanJob is one file to scan, from a zero cursor or, with old set, from
// old's cursor to append its tail.
type scanJob struct {
	path string
	// root, when set, requires core.SessionListable(root, path).
	root string
	// old is the indexed doc being appended to, or nil for a full scan.
	old *docEntry
}

// scanDocs runs jobs on buildWorkers goroutines. Results keep job order.
// progress, when set, is called from the worker goroutines, serialized, after
// each finished job. Only cancellation is returned as an error.
func scanDocs(ctx context.Context, jobs []scanJob, opts Options, progress func(done, total int)) ([]docBuild, error) {
	results := make([]docBuild, len(jobs))
	feed := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for range buildWorkers() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range feed {
				b := buildDoc(ctx, jobs[i], opts)
				mu.Lock()
				results[i] = b
				done++
				if progress != nil {
					progress(done, len(jobs))
				}
				mu.Unlock()
			}
		}()
	}
loop:
	for i := range jobs {
		select {
		case feed <- i:
		case <-ctx.Done():
			break loop
		}
	}
	close(feed)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// maxTitleRunes caps the stored session title.
const maxTitleRunes = 200

// docTitle returns the first non-empty line of s with whitespace collapsed,
// capped at maxTitleRunes.
func docTitle(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.Join(strings.Fields(line), " "); t != "" {
			return capRunes(t, maxTitleRunes)
		}
	}
	return ""
}

// buildDoc extracts one session. Files that vanish, are not listable, or
// fail to scan are marked skip (excluded when they exist but are not
// listable). An append job scans only the tail and reuses the old summary
// unless the tail may change it (meta, rename, compaction, or malformed rows),
// the session is a branch, or the old title is empty: in those cases the
// listability check and summary run again.
func buildDoc(ctx context.Context, job scanJob, opts Options) docBuild {
	path := job.path
	info, err := os.Stat(path)
	if err != nil {
		return docBuild{skip: true}
	}
	size := info.Size()
	excluded := func(scanned int64) docBuild {
		return docBuild{skip: true, scanned: scanned, doc: docEntry{SessionDoc: SessionDoc{Path: path, Updated: info.ModTime()}, Size: size, Excluded: true}}
	}
	var from core.SessionScanCursor
	if job.old != nil {
		from = job.old.Cursor
	}
	var scanned int64
	checked := false
	if job.old == nil && job.root != "" {
		scanned += size
		if !core.SessionListable(ctx, job.root, path) {
			return excluded(scanned)
		}
		checked = true
	}
	b := docBuild{terms: map[string][]uint32{}}
	// ScanSessionText decides a restart before emitting any row, so on
	// Reset every collected row belongs to the full rescan.
	cur, meta, err := core.ScanSessionText(ctx, path, from, func(row core.SessionTextRow) error {
		if opts.Skip != nil && opts.Skip(row.Message) {
			return nil
		}
		seq := uint32(row.MsgSeq)
		for _, text := range row.Texts {
			Tokenize(text, func(t string) {
				l := b.terms[t]
				if len(l) == 0 || l[len(l)-1] != seq {
					b.terms[t] = append(l, seq)
				}
			})
		}
		b.rows = append(b.rows, rowEntry{MsgID: seq, Offset: row.Offset, Len: row.Len, Role: row.Role, Time: row.Time, Fingerprint: row.Fingerprint})
		return nil
	})
	if err != nil {
		return docBuild{skip: true}
	}
	if cur.Reset {
		scanned += cur.Offset
	} else {
		scanned += cur.Offset - from.Offset
	}
	doc := docEntry{
		SessionDoc: SessionDoc{Path: path, ID: meta.ID, Updated: info.ModTime()},
		Size:       size,
		Cursor:     cur,
		TotalMsgs:  cur.MsgSeq,
	}
	doc.Cursor.Reset, doc.Cursor.SawMetaChange = false, false
	reuse := job.old != nil && !cur.Reset && !cur.SawMetaChange && meta.Parent == "" && job.old.Title != ""
	if reuse {
		doc.ID, doc.CWD, doc.Title, doc.Started = job.old.ID, job.old.CWD, job.old.Title, job.old.Started
	} else {
		if !checked && job.root != "" {
			scanned += size
			if !core.SessionListable(ctx, job.root, path) {
				return excluded(scanned)
			}
		}
		sum := core.DescribeSessionContext(ctx, path)
		scanned += 2 * size // snapshot read plus rename scan
		if ctx.Err() != nil {
			return docBuild{skip: true}
		}
		if sum.HideFromSessions {
			return excluded(scanned)
		}
		title := sum.Title
		if title == "" {
			title = sum.FirstUserText
		}
		doc.CWD, doc.Title, doc.Started = sum.CWD, docTitle(title), sum.Started
	}
	b.doc, b.reset, b.scanned = doc, cur.Reset, scanned
	return b
}

func (m *memIndex) nMsgs() int { return int(m.first[len(m.first)-1]) }

// runQuery evaluates q. ModeMessages: every token in one message.
// ModeSessions: every token somewhere in the session; hits are the
// session's messages that match at least one token. Sessions are ordered
// by Updated, newest first. A non-empty q.CWD keeps only that cwd. Rows
// are loaded only for the sessions kept after Limit.
func runQuery(ctx context.Context, src *segment, q Query) (Result, error) {
	tokens, _ := ParseQuery(q.Text)
	docs, first := src.docs, src.first
	var res Result
	if len(tokens) == 0 {
		for _, d := range recentDocs(docs, q.CWD, q.Limit) {
			res.Sessions = append(res.Sessions, SessionMatch{Doc: d})
		}
		return res, nil
	}
	if len(docs) == 0 {
		return res, nil
	}
	nMsgs := int(first[len(first)-1])
	sets := make([]bitset, len(tokens))
	for i, t := range tokens {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		bs, partial, err := src.tokenBits(t)
		if err != nil {
			return Result{}, err
		}
		sets[i] = bs
		res.Partial = res.Partial || partial
	}
	docOf := func(id uint32) int {
		return sort.Search(len(docs), func(d int) bool { return first[d+1] > id })
	}
	var hits bitset
	keepDoc := make([]bool, len(docs))
	for i := range keepDoc {
		keepDoc[i] = !docs[i].Excluded && sameCWD(q.CWD, docs[i].CWD)
	}
	if q.Mode == ModeMessages {
		hits = sets[0].clone()
		for _, s := range sets[1:] {
			hits.and(s)
		}
	} else {
		hits = newBitset(nMsgs)
		for _, s := range sets {
			present := make([]bool, len(docs))
			s.each(func(id uint32) { present[docOf(id)] = true })
			for d := range keepDoc {
				keepDoc[d] = keepDoc[d] && present[d]
			}
			hits.or(s)
		}
	}
	type docHits struct {
		d   int
		ids []uint32
	}
	byDoc := map[int]*docHits{}
	var order []*docHits
	hits.each(func(id uint32) {
		d := docOf(id)
		if !keepDoc[d] {
			return
		}
		h := byDoc[d]
		if h == nil {
			h = &docHits{d: d}
			byDoc[d] = h
			order = append(order, h)
		}
		h.ids = append(h.ids, id)
	})
	sort.Slice(order, func(i, j int) bool {
		a, b := docs[order[i].d], docs[order[j].d]
		if !a.Updated.Equal(b.Updated) {
			return a.Updated.After(b.Updated)
		}
		return a.Path < b.Path
	})
	if q.Limit > 0 && len(order) > q.Limit {
		order = order[:q.Limit]
	}
	for _, h := range order {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		doc := docs[h.d]
		rows, err := src.docRows(h.d)
		if err != nil {
			return Result{}, err
		}
		sm := SessionMatch{Doc: doc.SessionDoc}
		r := 0
		for _, id := range h.ids { // ids and rows are both ascending
			for r < len(rows) && rows[r].MsgID < id {
				r++
			}
			if r == len(rows) || rows[r].MsgID != id {
				continue
			}
			row := rows[r]
			sm.Count++
			sm.Hits = append(sm.Hits, HitRef{
				Path: doc.Path, MsgSeq: int(id - first[h.d]), TotalMsgs: doc.TotalMsgs,
				Fingerprint: row.Fingerprint, Role: row.Role, Time: row.Time, Offset: row.Offset, Len: row.Len,
			})
		}
		if sm.Count > 0 {
			res.Sessions = append(res.Sessions, sm)
		}
	}
	return res, nil
}

type bitset []uint64

func newBitset(n int) bitset   { return make(bitset, (n+63)/64) }
func (b bitset) set(i uint32)  { b[i>>6] |= 1 << (i & 63) }
func (b bitset) clone() bitset { return append(bitset(nil), b...) }
func (b bitset) and(o bitset) {
	for i := range b {
		b[i] &= o[i]
	}
}
func (b bitset) or(o bitset) {
	for i := range b {
		b[i] |= o[i]
	}
}
func (b bitset) each(fn func(uint32)) {
	for w, v := range b {
		for v != 0 {
			fn(uint32(w*64 + bits.TrailingZeros64(v)))
			v &= v - 1
		}
	}
}

// recentDocs returns non-excluded docs, newest Updated first (then path),
// filtered by cwd when non-empty and capped by limit when positive.
func recentDocs(docs []docEntry, cwd string, limit int) []SessionDoc {
	var out []SessionDoc
	for _, d := range docs {
		if !d.Excluded && sameCWD(cwd, d.CWD) {
			out = append(out, d.SessionDoc)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Updated.Equal(out[j].Updated) {
			return out[i].Updated.After(out[j].Updated)
		}
		return out[i].Path < out[j].Path
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// sameCWD reports whether docCWD matches the filter want; an empty filter
// matches everything. Paths are compared after filepath.Clean.
func sameCWD(want, docCWD string) bool {
	return want == "" || filepath.Clean(want) == filepath.Clean(docCWD)
}
