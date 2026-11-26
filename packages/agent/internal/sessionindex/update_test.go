package sessionindex

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/internal/flock"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func newRootSession(t *testing.T, root, cwd string, msgs ...provider.Message) string {
	t.Helper()
	s, err := core.NewSession(root, cwd, "p", "m", "v")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if err := s.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	path := s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendSession(t *testing.T, path string, msgs ...provider.Message) {
	t.Helper()
	s, _, err := core.OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if err := s.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func openTestIndex(t *testing.T, root string) *Index {
	t.Helper()
	x, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { x.Close() })
	return x
}

func mustUpdate(t *testing.T, x *Index) updateStats {
	t.Helper()
	st, err := x.update(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func hitPaths(t *testing.T, x *Index, text string) []string {
	t.Helper()
	r, err := x.Query(context.Background(), Query{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range r.Sessions {
		out = append(out, s.Doc.Path)
	}
	sort.Strings(out)
	return out
}

// searchDirContains reports whether any file in the index dir holds needle.
func searchDirContains(t *testing.T, x *Index, needle string) bool {
	t.Helper()
	entries, err := os.ReadDir(x.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(x.dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(needle)) {
			return true
		}
	}
	return false
}

func segFiles(t *testing.T, x *Index) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(x.dir, segPrefix+"*"+segSuffix))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestUpdateLifecycle(t *testing.T) {
	root := t.TempDir()
	x := openTestIndex(t, root)
	if r, err := x.Query(context.Background(), Query{Text: "alpha"}); err != nil || !r.Partial {
		t.Fatalf("query before build = %+v, %v", r, err)
	}
	a := newRootSession(t, root, "/proj/a", msg(provider.RoleUser, "alphaword zebramarker"))
	b := newRootSession(t, root, "/proj/b", msg(provider.RoleUser, "alphaword betaword"))

	st := mustUpdate(t, x)
	if !st.Wrote || !st.Rebuilt || st.Full != 2 {
		t.Fatalf("first update = %+v", st)
	}
	if got := hitPaths(t, x, "alpha"); len(got) != 2 {
		t.Fatalf("alpha = %v", got)
	}

	// No change: nothing written.
	gen := st.Gen
	if st = mustUpdate(t, x); st.Wrote || st.Gen != gen || st.Unchanged != 2 {
		t.Fatalf("idle update = %+v", st)
	}

	// Append: only the tail is scanned.
	before, _ := os.Stat(a)
	appendSession(t, a, msg(provider.RoleAssistant, "gammaword reply"))
	after, _ := os.Stat(a)
	st = mustUpdate(t, x)
	if st.Appended != 1 || st.Full != 0 || st.Unchanged != 1 || st.ScannedBytes != after.Size()-before.Size() {
		t.Fatalf("append update = %+v (tail %d)", st, after.Size()-before.Size())
	}
	if got := hitPaths(t, x, "gamma alpha"); len(got) != 1 || got[0] != a {
		t.Fatalf("gamma alpha = %v", got)
	}
	r, _ := x.Query(context.Background(), Query{Text: "gamma"})
	if h := r.Sessions[0].Hits[0]; h.MsgSeq != 1 || h.TotalMsgs != 2 {
		t.Fatalf("appended hit = %+v", h)
	}
	// Unchanged doc b kept its postings after renumbering.
	if got := hitPaths(t, x, "betaword"); len(got) != 1 || got[0] != b {
		t.Fatalf("beta = %v", got)
	}

	// Rewrite: replace a's bytes with another valid session of the same cwd.
	other := newRootSession(t, t.TempDir(), "/proj/a", msg(provider.RoleUser, "deltaword only"))
	raw, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	st = mustUpdate(t, x)
	if st.Full != 1 || st.Appended != 0 {
		t.Fatalf("rewrite update = %+v", st)
	}
	if got := hitPaths(t, x, "gamma"); len(got) != 0 {
		t.Fatalf("stale text after rewrite: %v", got)
	}
	if got := hitPaths(t, x, "delta"); len(got) != 1 {
		t.Fatalf("delta = %v", got)
	}
	if searchDirContains(t, x, "zebramarker") {
		t.Fatal("rewritten text still on disk")
	}

	// Deletion purges text.
	newRootSession(t, root, "/proj/c", msg(provider.RoleUser, "omegamarker text"))
	mustUpdate(t, x)
	if !searchDirContains(t, x, "omegamarker") {
		t.Fatal("marker not indexed")
	}
	cPaths := hitPaths(t, x, "omegamarker")
	if err := os.Remove(cPaths[0]); err != nil {
		t.Fatal(err)
	}
	if st = mustUpdate(t, x); st.Deleted != 1 {
		t.Fatalf("delete update = %+v", st)
	}
	if searchDirContains(t, x, "omegamarker") || len(hitPaths(t, x, "omegamarker")) != 0 {
		t.Fatal("deleted text still indexed")
	}
	if files := segFiles(t, x); len(files) != 1 {
		t.Fatalf("old generations left: %v", files)
	}
}

func TestForgetPurges(t *testing.T) {
	root := t.TempDir()
	x := openTestIndex(t, root)
	a := newRootSession(t, root, "/proj/a", msg(provider.RoleUser, "forgetmarker one"))
	newRootSession(t, root, "/proj/b", msg(provider.RoleUser, "keepword two"))
	mustUpdate(t, x)
	if err := x.Forget(a); err != nil {
		t.Fatal(err)
	}
	if searchDirContains(t, x, "forgetmarker") || len(hitPaths(t, x, "forgetmarker")) != 0 {
		t.Fatal("forgotten text still indexed")
	}
	if got := hitPaths(t, x, "keepword"); len(got) != 1 {
		t.Fatalf("keep = %v", got)
	}
	if st, err := x.forget("/nope.jsonl"); err != nil || st.Wrote {
		t.Fatalf("forget unknown = %+v, %v", st, err)
	}
}

func TestUpdateRebuildsCorruptSegment(t *testing.T) {
	root := t.TempDir()
	x := openTestIndex(t, root)
	newRootSession(t, root, "/proj/a", msg(provider.RoleUser, "alphaword"))
	st := mustUpdate(t, x)
	x.Close()
	path := filepath.Join(x.dir, segName(st.Gen))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-3] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := x.Query(context.Background(), Query{Text: "alpha"}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("query on corrupt = %v", err)
	}
	if st = mustUpdate(t, x); !st.Rebuilt || !st.Wrote {
		t.Fatalf("rebuild = %+v", st)
	}
	if got := hitPaths(t, x, "alpha"); len(got) != 1 {
		t.Fatalf("after rebuild = %v", got)
	}
	if err := os.WriteFile(filepath.Join(x.dir, currentName), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st = mustUpdate(t, x); !st.Rebuilt || len(segFiles(t, x)) != 1 {
		t.Fatalf("bad pointer rebuild = %+v", st)
	}
}

func TestUpdateBusy(t *testing.T) {
	root := t.TempDir()
	newRootSession(t, root, "/proj/a", msg(provider.RoleUser, "alphaword"))
	x := openTestIndex(t, root)
	mustUpdate(t, x)
	newRootSession(t, root, "/proj/b", msg(provider.RoleUser, "alphaword second"))

	lock, err := flock.TryLock(filepath.Join(x.dir, lockName))
	if err != nil {
		t.Fatal(err)
	}
	if err := x.Update(context.Background(), nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("update under lock = %v", err)
	}
	if err := x.Forget("x"); !errors.Is(err, ErrBusy) {
		t.Fatalf("forget under lock = %v", err)
	}
	if got := hitPaths(t, x, "alpha"); len(got) != 1 {
		t.Fatalf("stale query = %v", got)
	}
	lock.Close()

	// Two instances racing: each succeeds or reports busy; the result is
	// consistent and a reader on either instance sees the latest generation.
	y := openTestIndex(t, root)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, idx := range []*Index{x, y} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = idx.Update(context.Background(), nil)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil && !errors.Is(err, ErrBusy) {
			t.Fatal(err)
		}
	}
	mustUpdate(t, y)
	for _, idx := range []*Index{x, y} {
		if got := hitPaths(t, idx, "alpha"); len(got) != 2 {
			t.Fatalf("after race = %v", got)
		}
	}
	if files := segFiles(t, x); len(files) != 1 {
		t.Fatalf("generations = %v", files)
	}
}

// TestRealCorpus builds over a real sessions directory and logs aggregate
// numbers only. Run with ZUT_INDEX_CORPUS=<state>/sessions -v.
func TestRealCorpus(t *testing.T) {
	corpus := os.Getenv("ZUT_INDEX_CORPUS")
	if corpus == "" {
		t.Skip("ZUT_INDEX_CORPUS not set")
	}
	x := openTestIndex(t, filepath.Dir(filepath.Clean(corpus)))
	x.dir = t.TempDir() // never write into the real state dir

	start := time.Now()
	st := mustUpdate(t, x)
	build := time.Since(start)
	files := segFiles(t, x)
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	idle := mustUpdate(t, x)
	idleDur := time.Since(start)

	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	start = time.Now()
	if _, err := x.Query(context.Background(), Query{Text: "zz"}); err != nil {
		t.Fatal(err)
	}
	openDur := time.Since(start)
	runtime.GC()
	runtime.ReadMemStats(&m1)

	queries := []string{"refacto", "error test", "func return string", "fichier erreur", "session provider config",
		"re", "co de", "st in pr", "build", "commit branch"}
	for _, mode := range []Mode{ModeSessions, ModeMessages} {
		for _, limit := range []int{0, 50} {
			var ds []time.Duration
			for range 3 {
				for _, q := range queries {
					s := time.Now()
					if _, err := x.Query(context.Background(), Query{Text: q, Mode: mode, Limit: limit}); err != nil {
						t.Fatal(err)
					}
					ds = append(ds, time.Since(s))
				}
			}
			sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
			t.Logf("mode=%d limit=%d p50=%v p95=%v", mode, limit, ds[len(ds)/2], ds[len(ds)*95/100])
		}
	}
	t.Logf("docs=%d workers=%d build=%v segment=%.1fMB idleUpdate=%v (wrote=%v) open=%v heapDelta=%.1fMB",
		st.Full, buildWorkers(), build, float64(info.Size())/1e6, idleDur, idle.Wrote, openDur,
		(float64(m1.HeapInuse)-float64(m0.HeapInuse))/1e6)
}

func TestUpdateExcludedNotRescanned(t *testing.T) {
	root := t.TempDir()
	x := openTestIndex(t, root)
	newRootSession(t, root, "/proj/a", msg(provider.RoleUser, "alphaword"))
	hidden := newRootSession(t, root, "/proj/a", msg(provider.RoleUser, "hiddenmarker"))
	raw, err := os.ReadFile(hidden)
	if err != nil {
		t.Fatal(err)
	}
	patched := bytes.Replace(raw, []byte(`"meta":{`), []byte(`"meta":{"hide_from_sessions":true,`), 1)
	if err := os.WriteFile(hidden, patched, 0o600); err != nil {
		t.Fatal(err)
	}
	st := mustUpdate(t, x)
	if st.Full != 1 || st.Excluded != 1 {
		t.Fatalf("first = %+v", st)
	}
	if searchDirContains(t, x, "hiddenmarker") {
		t.Fatal("hidden text indexed")
	}
	if st = mustUpdate(t, x); st.Wrote || st.Unchanged != 2 || st.Excluded != 0 {
		t.Fatalf("idle = %+v", st)
	}
	if r, _ := x.Query(context.Background(), Query{Text: "zz"}); len(r.Sessions) != 0 {
		t.Fatal("excluded doc returned")
	}
}

func TestRecentAndEmptyQuery(t *testing.T) {
	root := t.TempDir()
	x := openTestIndex(t, root)
	a := newRootSession(t, root, "/proj/a", msg(provider.RoleUser, "one"))
	b := newRootSession(t, root, "/proj/b", msg(provider.RoleUser, "two"))
	c := newRootSession(t, root, "/proj/a", msg(provider.RoleUser, "three"))
	now := time.Now()
	for i, p := range []string{a, b, c} {
		mt := now.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	hidden := newRootSession(t, root, "/proj/a", msg(provider.RoleUser, "four"))
	raw, _ := os.ReadFile(hidden)
	if err := os.WriteFile(hidden, bytes.Replace(raw, []byte(`"meta":{`), []byte(`"meta":{"hide_from_sessions":true,`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, x)

	docPaths := func(docs []SessionDoc) []string {
		var out []string
		for _, d := range docs {
			out = append(out, d.Path)
		}
		return out
	}
	got, err := x.Recent(context.Background(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if p := docPaths(got); len(p) != 3 || p[0] != c || p[1] != b || p[2] != a {
		t.Fatalf("recent order wrong (%d)", len(p))
	}
	if got, _ = x.Recent(context.Background(), "/proj/a/", 1); len(got) != 1 || got[0].Path != c {
		t.Fatalf("recent cwd/limit = %v", docPaths(got))
	}
	for _, text := range []string{"", "  ", "@here"} {
		r, err := x.Query(context.Background(), Query{Text: text, CWD: "/proj/./a"})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Sessions) != 2 || r.Sessions[0].Doc.Path != c || r.Sessions[1].Doc.Path != a || len(r.Sessions[0].Hits) != 0 || r.Sessions[0].Count != 0 {
			t.Fatalf("empty query %q = %d sessions", text, len(r.Sessions))
		}
	}
	if r, _ := x.Query(context.Background(), Query{Text: "", Limit: 2}); len(r.Sessions) != 2 {
		t.Fatalf("empty query limit = %d", len(r.Sessions))
	}
	fresh := openTestIndex(t, t.TempDir())
	if got, err := fresh.Recent(context.Background(), "", 0); err != nil || got != nil {
		t.Fatalf("recent without index = %v, %v", got, err)
	}
}
