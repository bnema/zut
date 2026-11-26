package sessionindex

import (
	"context"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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

// memIndex is the in-memory index; B3 serializes it section by section.
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
	// scanned is the number of file bytes consumed by the scan.
	scanned int64
}

func buildWorkers() int { return max(1, runtime.NumCPU()/2) }

// scanJob is one file to scan, from a zero cursor or a resume cursor.
type scanJob struct {
	path string
	from core.SessionScanCursor
	// root, when set, requires core.SessionListable(root, path).
	root string
}

// buildMem indexes paths. Hidden sessions and unreadable files are left out.
func buildMem(ctx context.Context, paths []string, opts Options, progress func(done, total int)) (*memIndex, error) {
	jobs := make([]scanJob, len(paths))
	for i, p := range paths {
		jobs[i] = scanJob{path: p}
	}
	results, err := scanDocs(ctx, jobs, opts, progress)
	if err != nil {
		return nil, err
	}
	return mergeDocs(results), nil
}

// scanDocs runs jobs on buildWorkers goroutines. Results keep job order.
// Only cancellation is returned as an error.
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

// buildDoc extracts one session from cursor from. Files that vanish, are
// hidden, or fail to scan are marked skip. With a non-zero from, terms and
// rows hold only the tail, unless the scan restarted (reset).
func buildDoc(ctx context.Context, job scanJob, opts Options) docBuild {
	path, from := job.path, job.from
	info, err := os.Stat(path)
	if err != nil {
		return docBuild{skip: true}
	}
	excluded := docBuild{skip: true, doc: docEntry{SessionDoc: SessionDoc{Path: path, Updated: info.ModTime()}, Size: info.Size(), Excluded: true}}
	if job.root != "" && !core.SessionListable(ctx, job.root, path) {
		return excluded
	}
	sum := core.DescribeSessionContext(ctx, path)
	if ctx.Err() != nil {
		return docBuild{skip: true}
	}
	if sum.HideFromSessions {
		return excluded
	}
	title := sum.Title
	if title == "" {
		title = sum.FirstUserText
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
	b.reset = cur.Reset
	b.scanned = cur.Offset - from.Offset
	if cur.Reset {
		b.scanned = cur.Offset
	}
	b.doc = docEntry{
		SessionDoc: SessionDoc{Path: path, ID: meta.ID, CWD: sum.CWD, Title: title, Started: sum.Started, Updated: info.ModTime()},
		Size:       info.Size(),
		Cursor:     cur,
		TotalMsgs:  cur.MsgSeq,
	}
	b.doc.Cursor.Reset = false
	return b
}

// mergeDocs builds an index from fresh full scans.
func mergeDocs(results []docBuild) *memIndex {
	entries := make([]mergeEntry, 0, len(results))
	for _, r := range results {
		if !r.skip {
			entries = append(entries, mergeEntry{oldDoc: -1, doc: r.doc, add: r})
		}
	}
	m, _ := mergeEntries(nil, entries) // no old segment: cannot fail
	return m
}

func (m *memIndex) nMsgs() int { return int(m.first[len(m.first)-1]) }

// querySource is the read side shared by memIndex and segment, so both
// run the same query implementation.
type querySource interface {
	docList() []docEntry
	firstIDs() []uint32
	// tokenBits returns the bitset of global message IDs matching prefix and
	// whether the prefix expansion was capped.
	tokenBits(prefix string) (bitset, bool, error)
	// docRows returns the indexed rows of doc d sorted by MsgID.
	docRows(d int) ([]rowEntry, error)
}

func (m *memIndex) docList() []docEntry { return m.docs }
func (m *memIndex) firstIDs() []uint32  { return m.first }

func (m *memIndex) tokenBits(prefix string) (bitset, bool, error) {
	bs := newBitset(m.nMsgs())
	lo, hi, partial := PrefixRange(m.terms, prefix, maxPrefixTerms)
	for i := lo; i < hi; i++ {
		for _, id := range m.postings[i] {
			bs.set(id)
		}
	}
	return bs, partial, nil
}

func (m *memIndex) docRows(d int) ([]rowEntry, error) {
	return m.rows[m.rowFirst[d]:m.rowFirst[d+1]], nil
}

func (m *memIndex) query(ctx context.Context, q Query) (Result, error) {
	return runQuery(ctx, m, q)
}

// runQuery evaluates q. ModeMessages: every token in one message.
// ModeSessions: every token somewhere in the session; hits are the
// session's messages that match at least one token. Sessions are ordered
// by Updated, newest first. A non-empty q.CWD keeps only that cwd. Rows
// are loaded only for the sessions kept after Limit.
func runQuery(ctx context.Context, src querySource, q Query) (Result, error) {
	tokens, _ := ParseQuery(q.Text)
	docs, first := src.docList(), src.firstIDs()
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
