package sessionindex

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/internal/flock"
	"github.com/bnema/zut/packages/core"
)

// Index directory layout (all under <root>/search, mode 0700):
//
//	seg-<gen>.idx  immutable segment generations
//	current        decimal gen of the live segment, replaced by temp+rename
//	LOCK           writer lock (flock)
//
// Segments are never renamed over, so an open segment never blocks a
// writer on Windows. Old generations are removed best effort.
const (
	searchDirName = "search"
	currentName   = "current"
	lockName      = "LOCK"
	segPrefix     = "seg-"
	segSuffix     = ".idx"
)

// ErrBusy reports that another writer holds the index lock. The existing
// index stays queryable; call Update again later.
var ErrBusy = errors.New("sessionindex: another index update is running")

func segName(gen uint64) string { return fmt.Sprintf("%s%d%s", segPrefix, gen, segSuffix) }

// mergeEntry is one doc of the next generation. oldDoc >= 0 keeps that
// old doc's postings and rows (remapped); add holds extra terms and rows
// with local message seqs (a tail, or the whole doc when oldDoc < 0).
type mergeEntry struct {
	oldDoc int
	doc    docEntry
	add    docBuild
}

// mergeEntries builds the next index. Old docs not referenced by an entry
// are dropped, and terms left without postings disappear. Kept old docs
// must stay in their old relative order.
func mergeEntries(old *segment, entries []mergeEntry) (*memIndex, error) {
	m := &memIndex{first: []uint32{0}, rowFirst: []uint32{0}}
	var newBase []int64
	if old != nil {
		newBase = make([]int64, len(old.docs))
		for i := range newBase {
			newBase[i] = -1
		}
	}
	added := map[string][]uint32{}
	for _, e := range entries {
		base := m.first[len(m.first)-1]
		if e.oldDoc >= 0 {
			newBase[e.oldDoc] = int64(base)
			rows, err := old.docRows(e.oldDoc)
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				r.MsgID = r.MsgID - old.first[e.oldDoc] + base
				m.rows = append(m.rows, r)
			}
		}
		for t, seqs := range e.add.terms {
			g := added[t]
			for _, s := range seqs {
				g = append(g, base+s)
			}
			added[t] = g
		}
		for _, r := range e.add.rows {
			r.MsgID += base
			m.rows = append(m.rows, r)
		}
		m.docs = append(m.docs, e.doc)
		m.first = append(m.first, base+uint32(e.doc.TotalMsgs))
		m.rowFirst = append(m.rowFirst, uint32(len(m.rows)))
	}
	// Entries were visited in order, so every added list is ascending.

	var oldTerms []string
	var oldPost [][]uint32
	if old != nil {
		buf, err := old.readPostings()
		if err != nil {
			return nil, err
		}
		nMsgs := old.first[len(old.first)-1]
		for i := 0; i+1 < len(old.termOffs); i++ {
			d := buf[old.postOffs[i]:old.postOffs[i+1]]
			var ids []uint32
			id, doc := uint64(0), 0
			for len(d) > 0 {
				v, k := binary.Uvarint(d)
				if k <= 0 {
					return nil, corrupt("postings")
				}
				d = d[k:]
				if v >= uint64(nMsgs) { // also rules out wraparound
					return nil, corrupt("postings")
				}
				id += v
				if id >= uint64(nMsgs) {
					return nil, corrupt("postings")
				}
				for uint64(old.first[doc+1]) <= id {
					doc++
				}
				if b := newBase[doc]; b >= 0 {
					ids = append(ids, uint32(b)+uint32(id)-old.first[doc])
				}
			}
			t := string(old.term(i))
			if extra, ok := added[t]; ok {
				ids = mergeSorted(ids, extra)
				delete(added, t)
			}
			if len(ids) > 0 {
				oldTerms = append(oldTerms, t)
				oldPost = append(oldPost, ids)
			}
		}
	}
	newTerms := make([]string, 0, len(added))
	for t := range added {
		newTerms = append(newTerms, t)
	}
	sort.Strings(newTerms)
	m.terms = make([]string, 0, len(oldTerms)+len(newTerms))
	m.postings = make([][]uint32, 0, len(oldTerms)+len(newTerms))
	i, j := 0, 0
	for i < len(oldTerms) || j < len(newTerms) {
		if j == len(newTerms) || (i < len(oldTerms) && oldTerms[i] < newTerms[j]) {
			m.terms, m.postings = append(m.terms, oldTerms[i]), append(m.postings, oldPost[i])
			i++
		} else {
			m.terms, m.postings = append(m.terms, newTerms[j]), append(m.postings, added[newTerms[j]])
			j++
		}
	}
	return m, nil
}

func mergeSorted(a, b []uint32) []uint32 {
	out := make([]uint32, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] <= b[j] {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	return append(append(out, a[i:]...), b[j:]...)
}

// readPostings reads the whole postings section.
func (s *segment) readPostings() ([]byte, error) {
	buf := make([]byte, s.postOffs[len(s.postOffs)-1])
	if _, err := s.f.ReadAt(buf, s.postBase); err != nil {
		return nil, fmt.Errorf("read session index postings: %w", err)
	}
	return buf, nil
}

// updateStats describes one Update or Forget run.
type updateStats struct {
	Unchanged, Appended, Full, Deleted int
	// Excluded counts scanned files found not listable (kept as markers).
	Excluded int
	// ScannedBytes is the number of session bytes read by text scans.
	ScannedBytes int64
	Rebuilt      bool // old index missing or corrupt
	Wrote        bool
	Gen          uint64
}

func readCurrent(dir string) (uint64, error) {
	b, err := os.ReadFile(filepath.Join(dir, currentName))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read session index pointer: %w", err)
	}
	gen, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || gen == 0 {
		return 0, corrupt("current pointer")
	}
	return gen, nil
}

// writeCurrent publishes gen through a temp file and rename. On Windows the
// rename fails while another process is reading `current`, so it is retried a
// few times with a short backoff (bounded, ctx-aware).
func writeCurrent(ctx context.Context, dir string, gen uint64) error {
	tmp, err := os.CreateTemp(dir, ".current-*.tmp")
	if err != nil {
		return fmt.Errorf("write session index pointer: %w", err)
	}
	name := tmp.Name()
	_, err = tmp.WriteString(strconv.FormatUint(gen, 10))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = renameRetry(ctx, name, filepath.Join(dir, currentName))
	}
	if err != nil {
		os.Remove(name)
		return fmt.Errorf("write session index pointer: %w", err)
	}
	return nil
}

func renameRetry(ctx context.Context, from, to string) error {
	delay := 10 * time.Millisecond
	for attempt := 0; ; attempt++ {
		err := os.Rename(from, to)
		if err == nil || attempt == 4 {
			return err
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return errors.Join(err, ctx.Err())
		case <-t.C:
		}
		delay *= 2
	}
}

// maxGen returns the highest generation among segment files in dir.
func maxGen(dir string) uint64 {
	entries, _ := os.ReadDir(dir)
	var g uint64
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, segPrefix) && strings.HasSuffix(n, segSuffix) {
			if v, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(n, segPrefix), segSuffix), 10, 64); err == nil && v > g {
				g = v
			}
		}
	}
	return g
}

// cleanup removes old generations and temp files. Errors are ignored.
// Windows limitation: a generation still open in another process (Go opens
// files without FILE_SHARE_DELETE) cannot be removed, so its text stays on
// disk until a later locked Update or Forget, which call cleanup first.
func cleanup(dir string, keep uint64) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		n := e.Name()
		stale := strings.HasSuffix(n, ".tmp") ||
			(strings.HasPrefix(n, segPrefix) && strings.HasSuffix(n, segSuffix) && n != segName(keep))
		if stale {
			_ = os.Remove(filepath.Join(dir, n))
		}
	}
}

// openBase opens the live segment for a writer. A missing or corrupt index
// returns nil so the caller rebuilds.
func openBase(dir string) (*segment, uint64, error) {
	gen, err := readCurrent(dir)
	if errors.Is(err, ErrCorrupt) {
		return nil, 0, nil
	}
	if err != nil || gen == 0 {
		return nil, 0, err
	}
	seg, err := openSegment(filepath.Join(dir, segName(gen)))
	if errors.Is(err, ErrCorrupt) || errors.Is(err, os.ErrNotExist) {
		return nil, gen, nil
	}
	if err != nil {
		return nil, 0, err
	}
	return seg, gen, nil
}

// update brings the index in line with the listable sessions under root.
func (x *Index) update(ctx context.Context, progress func(done, total int)) (updateStats, error) {
	var st updateStats
	lock, err := flock.TryLock(filepath.Join(x.dir, lockName))
	if errors.Is(err, flock.ErrBusy) {
		return st, ErrBusy
	}
	if err != nil {
		return st, fmt.Errorf("update session index: %w", err)
	}
	defer lock.Close()

	old, gen, err := openBase(x.dir)
	if err != nil {
		return st, fmt.Errorf("update session index: %w", err)
	}
	if old == nil {
		st.Rebuilt = true
	} else {
		defer old.Close()
	}
	cleanup(x.dir, gen) // retry removals that failed earlier (Windows)
	files, err := core.SessionFiles(ctx, x.root)
	if err != nil {
		// Never mistake an unreadable bucket for deleted sessions.
		return st, fmt.Errorf("update session index: %w", err)
	}
	m, err := x.plan(ctx, old, files, progress, &st)
	if errors.Is(err, ErrCorrupt) && old != nil {
		// The base failed while merging: rebuild everything.
		st = updateStats{Rebuilt: true}
		m, err = x.plan(ctx, nil, files, progress, &st)
	}
	if err != nil || m == nil {
		st.Gen = gen
		return st, err
	}
	return st, x.commit(ctx, m, gen, &st)
}

// plan scans what changed and merges it with old. It returns a nil index
// when nothing changed. Eligibility (core.SessionListable) is checked only
// for new or changed files: an unchanged file keeps its earlier verdict, and
// any change that could alter it (meta rows, hide flag) changes the file.
func (x *Index) plan(ctx context.Context, old *segment, files []core.SessionFile, progress func(done, total int), st *updateStats) (*memIndex, error) {
	oldByPath := map[string]int{}
	if old != nil {
		for i, d := range old.docs {
			oldByPath[d.Path] = i
		}
	}
	type slot struct {
		oldDoc int // -1 for new docs
		job    int // -1 when unchanged
	}
	var jobs []scanJob
	slots := make([]slot, 0, len(files))
	seen := map[int]bool{}
	for _, f := range files {
		p, info := f.Path, f.Info
		d, ok := oldByPath[p]
		if !ok {
			slots = append(slots, slot{oldDoc: -1, job: len(jobs)})
			jobs = append(jobs, scanJob{path: p, root: x.root})
			continue
		}
		seen[d] = true
		od := old.docs[d]
		switch {
		case info.Size() == od.Size && info.ModTime().Equal(od.Updated):
			slots = append(slots, slot{oldDoc: d, job: -1})
		case info.Size() > od.Size:
			slots = append(slots, slot{oldDoc: d, job: len(jobs)})
			jobs = append(jobs, scanJob{path: p, root: x.root, old: &od})
		default:
			slots = append(slots, slot{oldDoc: d, job: len(jobs)})
			jobs = append(jobs, scanJob{path: p, root: x.root})
		}
	}
	if old != nil {
		st.Deleted = len(old.docs) - len(seen)
	}
	if old != nil && len(jobs) == 0 && st.Deleted == 0 {
		st.Unchanged = len(slots)
		return nil, nil
	}
	results, err := scanDocs(ctx, jobs, x.opts, progress)
	if err != nil {
		return nil, err
	}
	// Kept old docs must stay in old order; slots follow listing order.
	sort.SliceStable(slots, func(i, j int) bool {
		a, b := slots[i].oldDoc, slots[j].oldDoc
		return a >= 0 && (b < 0 || a < b)
	})
	entries := make([]mergeEntry, 0, len(slots))
	for _, s := range slots {
		if s.job < 0 {
			// Re-stat right before merging: a session deleted (and possibly
			// Forgotten, which got ErrBusy) while this update scanned must not
			// be republished.
			if _, err := os.Stat(old.docs[s.oldDoc].Path); errors.Is(err, os.ErrNotExist) {
				st.Deleted++
				continue
			}
			st.Unchanged++
			entries = append(entries, mergeEntry{oldDoc: s.oldDoc, doc: old.docs[s.oldDoc]})
			continue
		}
		r := results[s.job]
		st.ScannedBytes += r.scanned
		if r.skip {
			if r.doc.Excluded {
				st.Excluded++
				entries = append(entries, mergeEntry{oldDoc: -1, doc: r.doc})
			}
			continue
		}
		e := mergeEntry{oldDoc: -1, doc: r.doc, add: r}
		if jobs[s.job].old != nil && !r.reset {
			e.oldDoc = s.oldDoc
			st.Appended++
		} else {
			st.Full++
		}
		entries = append(entries, e)
	}
	return mergeEntries(old, entries)
}

// commit writes m as the next generation and publishes it.
func (x *Index) commit(ctx context.Context, m *memIndex, gen uint64, st *updateStats) error {
	next := max(gen, maxGen(x.dir)) + 1
	if err := writeSegment(filepath.Join(x.dir, segName(next)), m); err != nil {
		return fmt.Errorf("update session index: %w", err)
	}
	if err := writeCurrent(ctx, x.dir, next); err != nil {
		os.Remove(filepath.Join(x.dir, segName(next)))
		return fmt.Errorf("update session index: %w", err)
	}
	st.Wrote, st.Gen = true, next
	// Drop this Index's handle on the old generation so it can be removed.
	x.mu.Lock()
	if x.seg != nil && x.segGen != next {
		x.seg.Close()
		x.seg, x.segGen = nil, 0
	}
	x.mu.Unlock()
	cleanup(x.dir, next)
	return nil
}

// forget removes path from the index and publishes a new generation.
func (x *Index) forget(path string) (updateStats, error) {
	var st updateStats
	lock, err := flock.TryLock(filepath.Join(x.dir, lockName))
	if errors.Is(err, flock.ErrBusy) {
		return st, ErrBusy
	}
	if err != nil {
		return st, fmt.Errorf("forget session: %w", err)
	}
	defer lock.Close()
	old, gen, err := openBase(x.dir)
	if err != nil || old == nil {
		return st, err
	}
	defer old.Close()
	cleanup(x.dir, gen) // retry removals that failed earlier (Windows)
	entries := make([]mergeEntry, 0, len(old.docs))
	for i, d := range old.docs {
		if d.Path == path {
			st.Deleted++
			continue
		}
		entries = append(entries, mergeEntry{oldDoc: i, doc: d})
	}
	if st.Deleted == 0 {
		st.Gen = gen
		return st, nil
	}
	m, err := mergeEntries(old, entries)
	if err != nil {
		return st, fmt.Errorf("forget session: %w", err)
	}
	return st, x.commit(context.Background(), m, gen, &st)
}
