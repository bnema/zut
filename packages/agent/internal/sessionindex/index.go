// Package sessionindex maintains a token-prefix inverted index over the
// user and assistant text of persisted zut sessions.
package sessionindex

import (
	"context"
	"errors"
	"time"

	"github.com/bnema/zut/packages/provider"
)

var errNotImplemented = errors.New("sessionindex: not implemented")

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
	// Partial is set when prefix expansion was capped or the index is stale.
	Partial bool
}

// Snippet is the text of one hit, read back from its session file.
type Snippet struct {
	Ref  HitRef
	Text string
}

// Index is an on-disk session text index.
type Index struct {
	root string
	opts Options
}

// Open opens or creates the index for the session root.
func Open(root string, opts Options) (*Index, error) {
	return &Index{root: root, opts: opts}, nil
}

// Close releases the index.
func (x *Index) Close() error { return nil }

// Update reindexes changed sessions, reporting progress.
func (x *Index) Update(ctx context.Context, progress func(done, total int)) error {
	return errNotImplemented
}

// Query runs q against the index.
func (x *Index) Query(ctx context.Context, q Query) (Result, error) {
	return Result{}, errNotImplemented
}

// Snippets reads the text of refs from their session files.
func (x *Index) Snippets(ctx context.Context, refs []HitRef) ([]Snippet, error) {
	return nil, errNotImplemented
}

// Forget drops path from the index.
func (x *Index) Forget(path string) error { return errNotImplemented }
