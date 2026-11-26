package sessionindex

import (
	"context"
	"math/bits"
	"os"
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
}

func buildWorkers() int { return max(1, runtime.NumCPU()/2) }

// buildMem indexes paths. Hidden sessions and unreadable files are left out.
func buildMem(ctx context.Context, paths []string, opts Options, progress func(done, total int)) (*memIndex, error) {
	results := make([]docBuild, len(paths))
	jobs := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	done := 0
	for range buildWorkers() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				b, err := buildDoc(ctx, paths[i], opts)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				results[i] = b
				done++
				if progress != nil {
					progress(done, len(paths))
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for i := range paths {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return mergeDocs(results), nil
}

// buildDoc extracts one session. Only cancellation is returned as an error;
// files that vanish or fail to scan are marked skip.
func buildDoc(ctx context.Context, path string, opts Options) (docBuild, error) {
	info, err := os.Stat(path)
	if err != nil {
		return docBuild{skip: true}, nil
	}
	sum := core.DescribeSessionContext(ctx, path)
	if err := ctx.Err(); err != nil {
		return docBuild{}, err
	}
	if sum.HideFromSessions {
		return docBuild{skip: true}, nil
	}
	title := sum.Title
	if title == "" {
		title = sum.FirstUserText
	}
	b := docBuild{terms: map[string][]uint32{}}
	cur, meta, err := core.ScanSessionText(ctx, path, core.SessionScanCursor{}, func(row core.SessionTextRow) error {
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
		if ctxErr := ctx.Err(); ctxErr != nil {
			return docBuild{}, ctxErr
		}
		return docBuild{skip: true}, nil
	}
	b.doc = docEntry{
		SessionDoc: SessionDoc{Path: path, ID: meta.ID, CWD: sum.CWD, Title: title, Started: sum.Started, Updated: info.ModTime()},
		Size:       info.Size(),
		Cursor:     cur,
		TotalMsgs:  cur.MsgSeq,
	}
	return b, nil
}

func mergeDocs(results []docBuild) *memIndex {
	m := &memIndex{first: []uint32{0}, rowFirst: []uint32{0}}
	all := map[string][]uint32{}
	for _, r := range results {
		if r.skip {
			continue
		}
		base := m.first[len(m.first)-1]
		for t, seqs := range r.terms {
			g := all[t]
			for _, s := range seqs {
				g = append(g, base+s)
			}
			all[t] = g
		}
		for _, row := range r.rows {
			row.MsgID += base
			m.rows = append(m.rows, row)
		}
		m.docs = append(m.docs, r.doc)
		m.first = append(m.first, base+uint32(r.doc.TotalMsgs))
		m.rowFirst = append(m.rowFirst, uint32(len(m.rows)))
	}
	m.terms = make([]string, 0, len(all))
	for t := range all {
		m.terms = append(m.terms, t)
	}
	sort.Strings(m.terms)
	m.postings = make([][]uint32, len(m.terms))
	for i, t := range m.terms {
		m.postings[i] = all[t]
	}
	return m
}

func (m *memIndex) nMsgs() int { return int(m.first[len(m.first)-1]) }

// tokenBits returns the bitset of global message IDs matching prefix.
func (m *memIndex) tokenBits(prefix string) (bitset, bool) {
	bs := newBitset(m.nMsgs())
	lo, hi, partial := PrefixRange(m.terms, prefix, maxPrefixTerms)
	for i := lo; i < hi; i++ {
		for _, id := range m.postings[i] {
			bs.set(id)
		}
	}
	return bs, partial
}

// docOf returns the doc index owning global message id.
func (m *memIndex) docOf(id uint32) int {
	return sort.Search(len(m.docs), func(d int) bool { return m.first[d+1] > id })
}

// rowOf returns the indexed row of global message id.
func (m *memIndex) rowOf(id uint32) (rowEntry, bool) {
	i := sort.Search(len(m.rows), func(i int) bool { return m.rows[i].MsgID >= id })
	if i < len(m.rows) && m.rows[i].MsgID == id {
		return m.rows[i], true
	}
	return rowEntry{}, false
}

// query evaluates q. ModeMessages: every token in one message.
// ModeSessions: every token somewhere in the session; hits are the
// session's messages that match at least one token. Sessions are ordered
// by Updated, newest first. A non-empty q.CWD keeps only that cwd.
func (m *memIndex) query(ctx context.Context, q Query) (Result, error) {
	tokens, _ := ParseQuery(q.Text)
	var res Result
	if len(tokens) == 0 || len(m.docs) == 0 {
		return res, nil
	}
	sets := make([]bitset, len(tokens))
	for i, t := range tokens {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		bs, partial := m.tokenBits(t)
		sets[i] = bs
		res.Partial = res.Partial || partial
	}
	var hits bitset
	keepDoc := make([]bool, len(m.docs))
	if q.Mode == ModeMessages {
		hits = sets[0].clone()
		for _, s := range sets[1:] {
			hits.and(s)
		}
		for i := range keepDoc {
			keepDoc[i] = true
		}
	} else {
		hits = newBitset(m.nMsgs())
		for i := range keepDoc {
			keepDoc[i] = true
		}
		for _, s := range sets {
			present := make([]bool, len(m.docs))
			s.each(func(id uint32) { present[m.docOf(id)] = true })
			for d := range keepDoc {
				keepDoc[d] = keepDoc[d] && present[d]
			}
			hits.or(s)
		}
	}
	byDoc := map[int]*SessionMatch{}
	hits.each(func(id uint32) {
		d := m.docOf(id)
		if !keepDoc[d] || (q.CWD != "" && m.docs[d].CWD != q.CWD) {
			return
		}
		row, ok := m.rowOf(id)
		if !ok {
			return
		}
		sm := byDoc[d]
		if sm == nil {
			sm = &SessionMatch{Doc: m.docs[d].SessionDoc}
			byDoc[d] = sm
		}
		sm.Count++
		sm.Hits = append(sm.Hits, HitRef{
			Path: m.docs[d].Path, MsgSeq: int(id - m.first[d]), TotalMsgs: m.docs[d].TotalMsgs,
			Fingerprint: row.Fingerprint, Role: row.Role, Time: row.Time, Offset: row.Offset, Len: row.Len,
		})
	})
	for _, sm := range byDoc {
		res.Sessions = append(res.Sessions, *sm)
	}
	sort.Slice(res.Sessions, func(i, j int) bool {
		a, b := res.Sessions[i].Doc, res.Sessions[j].Doc
		if !a.Updated.Equal(b.Updated) {
			return a.Updated.After(b.Updated)
		}
		return a.Path < b.Path
	})
	if q.Limit > 0 && len(res.Sessions) > q.Limit {
		res.Sessions = res.Sessions[:q.Limit]
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
