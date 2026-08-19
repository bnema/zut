package modes

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

const sessionDialogTestTimeout = 10 * time.Second

func TestFormatSessionRowPlainSanitizesControlBytes(t *testing.T) {
	row := formatSessionRowPlain(core.SessionSummary{
		Provider:      "test\x1b]0;bad\a",
		Model:         "model\x1b[31m",
		MessageCount:  1,
		FirstUserText: "hello\x1b[2J\nworld",
	}, 120)
	if strings.IndexFunc(row, unicode.IsControl) >= 0 {
		t.Fatalf("session row contains control characters: %q", row)
	}
	if !strings.Contains(row, "hello world") {
		t.Fatalf("sanitized session text missing: %q", row)
	}
}

func TestSessionDialogLoadsEntriesWithoutBlockingOpen(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	session, err := core.NewSession(root, cwd, "test", "test-model", "test-version")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "load this session"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	d := newSessionDialog()
	events := d.Open(context.Background(), root, cwd)
	t.Cleanup(d.Close)
	if !d.Active() || !d.Loading() {
		t.Fatalf("dialog state after Open = active %v, loading %v; want active and loading", d.Active(), d.Loading())
	}
	loadingText := strings.Join(d.Render(tui.Dark, 100), "\n")
	if !strings.Contains(loadingText, "Loading sessions") {
		t.Fatalf("loading render = %q, want spinner status", loadingText)
	}
	act := d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if act.Select || !d.Active() {
		t.Fatalf("enter while loading = %+v, active %v; want no selection and active dialog", act, d.Active())
	}

	for event := range events {
		d.ApplyLoad(event)
	}

	if d.Loading() {
		t.Fatal("dialog still loading after final result")
	}
	if len(d.sessions) != 1 || d.sessions[0].Path != session.Path {
		t.Fatalf("loaded sessions = %+v, want %q", d.sessions, session.Path)
	}
	loadedText := strings.Join(d.Render(tui.Dark, 100), "\n")
	if strings.Contains(loadedText, "Loading sessions") {
		t.Fatalf("completed render still shows loading spinner: %q", loadedText)
	}
	if !strings.Contains(loadedText, "load this session") {
		t.Fatalf("completed render missing session text: %q", loadedText)
	}
}

func TestSessionDialogCanceledParentDoesNotRemainLoading(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := newSessionDialog()
	events := d.Open(ctx, t.TempDir(), t.TempDir())
	if d.Loading() {
		t.Fatal("dialog remained loading with an already-canceled parent")
	}
	if d.Active() {
		t.Fatal("dialog remained active with an already-canceled parent")
	}
	if _, ok := <-events; ok {
		t.Fatal("canceled dialog emitted a load event")
	}
}

func TestSessionDialogCancellationFinishesLoading(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := newSessionDialog()
	events := d.Open(ctx, t.TempDir(), t.TempDir())
	cancel()
	for range events {
	}
	d.ApplyLoadClosed()
	if d.Loading() {
		t.Fatal("dialog remained loading after parent cancellation")
	}
	if d.Active() {
		t.Fatal("dialog remained active after parent cancellation")
	}
}

func TestSessionDialogCancellationDoesNotEmitFinished(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	session, err := core.NewSession(root, cwd, "test", "test-model", "test-version")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "cancel this load"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	d := newSessionDialog()
	d.describeSession = func(ctx context.Context, path string) core.SessionSummary {
		close(entered)
		<-ctx.Done()
		return core.SessionSummary{Path: path}
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := d.Open(ctx, root, cwd)
	t.Cleanup(func() {
		cancel()
		d.Close()
	})

	select {
	case event, ok := <-events:
		if !ok || event.kind != sessionLoadStarted {
			t.Fatalf("first load event = %+v, open %v; want sessionLoadStarted", event, ok)
		}
		d.ApplyLoad(event)
	case <-time.After(sessionDialogTestTimeout):
		t.Fatal("timed out waiting for sessionLoadStarted")
	}
	select {
	case <-entered:
	case <-time.After(sessionDialogTestTimeout):
		t.Fatal("timed out waiting for the session worker")
	}

	cancel()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				d.ApplyLoadClosed()
				if d.Loading() || d.Active() {
					t.Fatalf("dialog after cancellation = loading %v, active %v; want both false", d.Loading(), d.Active())
				}
				return
			}
			if event.kind == sessionLoadFinished {
				t.Fatal("canceled load emitted sessionLoadFinished")
			}
			d.ApplyLoad(event)
		case <-time.After(sessionDialogTestTimeout):
			t.Fatal("timed out waiting for canceled session load to close")
		}
	}
}

func TestSessionDialogPreCanceledOpenPreservesPreviousLoadBarrier(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	session, err := core.NewSession(root, cwd, "test", "test-model", "test-version")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "wait for the prior load"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	releaseLoad := func() { releaseOnce.Do(func() { close(release) }) }
	d := newSessionDialog()
	d.describeSession = func(ctx context.Context, path string) core.SessionSummary {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return core.SessionSummary{Path: path, MessageCount: 1}
	}
	firstCtx, firstCancel := context.WithCancel(context.Background())
	firstEvents := d.Open(firstCtx, root, cwd)
	t.Cleanup(func() {
		firstCancel()
		releaseLoad()
		d.Close()
	})

	select {
	case event, ok := <-firstEvents:
		if !ok || event.kind != sessionLoadStarted {
			t.Fatalf("first load event = %+v, open %v; want sessionLoadStarted", event, ok)
		}
		d.ApplyLoad(event)
	case <-time.After(sessionDialogTestTimeout):
		t.Fatal("timed out waiting for the first load to start")
	}
	select {
	case <-entered:
	case <-time.After(sessionDialogTestTimeout):
		t.Fatal("timed out waiting for the first worker")
	}

	preCanceled, cancelPreCanceled := context.WithCancel(context.Background())
	cancelPreCanceled()
	preCanceledEvents := d.Open(preCanceled, root, cwd)
	if _, ok := <-preCanceledEvents; ok {
		t.Fatal("pre-canceled open emitted a load event")
	}

	thirdCtx, cancelThird := context.WithCancel(context.Background())
	thirdEvents := d.Open(thirdCtx, root, cwd)
	select {
	case event, ok := <-thirdEvents:
		t.Fatalf("third open started before the first load finished: %+v, open %v", event, ok)
	case <-time.After(100 * time.Millisecond):
	}

	releaseLoad()
	firstCancel()
	select {
	case event, ok := <-thirdEvents:
		if !ok || event.kind != sessionLoadStarted {
			t.Fatalf("third load event = %+v, open %v; want sessionLoadStarted after barrier", event, ok)
		}
		d.ApplyLoad(event)
	case <-time.After(sessionDialogTestTimeout):
		t.Fatal("timed out waiting for third load after releasing the first")
	}
	cancelThird()
	for event := range thirdEvents {
		d.ApplyLoad(event)
	}
}

func TestSessionDialogEscapeCancelsLoading(t *testing.T) {
	d := newSessionDialog()
	events := d.Open(context.Background(), t.TempDir(), t.TempDir())
	act := d.HandleKey(tui.Key{Kind: tui.KeyEsc})
	if !act.Close || d.Active() || d.Loading() {
		t.Fatalf("escape action = %+v, active %v, loading %v; want closed dialog", act, d.Active(), d.Loading())
	}
	for range events {
	}
}

func TestSessionDialogAppliesLoadedEntriesIncrementallyInPathOrder(t *testing.T) {
	d := newSessionDialog()
	d.active = true
	d.loading = true
	d.loadGeneration = 7

	d.ApplyLoad(sessionLoadEvent{
		kind:       sessionLoadStarted,
		generation: 7,
		total:      4,
	})
	d.ApplyLoad(sessionLoadEvent{
		kind:       sessionLoadEntry,
		generation: 7,
		index:      2,
		summary:    core.SessionSummary{Path: "third", MessageCount: 1},
	})
	if len(d.sessions) != 0 {
		t.Fatalf("older out-of-order entry appeared before newest entry: %+v", d.sessions)
	}
	d.ApplyLoad(sessionLoadEvent{
		kind:       sessionLoadEntry,
		generation: 7,
		index:      1,
		summary:    core.SessionSummary{Path: "hidden", MessageCount: 1, HideFromSessions: true},
	})
	d.ApplyLoad(sessionLoadEvent{
		kind:       sessionLoadEntry,
		generation: 7,
		index:      3,
		summary:    core.SessionSummary{Path: "empty"},
	})
	d.ApplyLoad(sessionLoadEvent{
		kind:       sessionLoadEntry,
		generation: 7,
		index:      0,
		summary:    core.SessionSummary{Path: "first", MessageCount: 1},
	})
	if !d.Loading() {
		t.Fatal("dialog stopped loading before the terminal event")
	}
	if len(d.sessions) != 2 || d.sessions[0].Path != "first" || d.sessions[1].Path != "third" {
		t.Fatalf("incremental sessions = %+v, want first then third", d.sessions)
	}

	d.ApplyLoad(sessionLoadEvent{kind: sessionLoadFinished, generation: 7})

	if d.Loading() {
		t.Fatal("dialog still loading after final result")
	}
	if len(d.sessions) != 2 || d.sessions[0].Path != "first" || d.sessions[1].Path != "third" {
		t.Fatalf("loaded sessions = %+v, want first then third", d.sessions)
	}
}

func TestSessionDialogCanSelectLoadedEntryWhileLoading(t *testing.T) {
	d := newSessionDialog()
	d.active = true
	d.loading = true
	d.loadGeneration = 9

	d.ApplyLoad(sessionLoadEvent{
		kind:       sessionLoadStarted,
		generation: 9,
		total:      2,
	})
	d.ApplyLoad(sessionLoadEvent{
		kind:       sessionLoadEntry,
		generation: 9,
		index:      0,
		summary:    core.SessionSummary{Path: "newest", MessageCount: 1, FirstUserText: "recent work"},
	})

	if !d.Loading() {
		t.Fatal("dialog stopped loading after first entry")
	}
	rendered := strings.Join(d.Render(tui.Dark, 100), "\n")
	if !strings.Contains(rendered, "recent work") {
		t.Fatalf("incremental render missing newest entry: %q", rendered)
	}

	act := d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if !act.Select || act.Path != "newest" || d.Active() {
		t.Fatalf("select while loading = %+v, active %v; want newest selected and dialog closed", act, d.Active())
	}
}

func TestSessionDialogAllScopeIncludesOtherCWDBuckets(t *testing.T) {
	root := t.TempDir()
	cwdA := t.TempDir()
	cwdB := t.TempDir()
	for _, cwd := range []string{cwdA, cwdB} {
		session, err := core.NewSession(root, cwd, "test", "model", "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := session.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "scope"}}}); err != nil {
			t.Fatal(err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	d := newSessionDialog()
	events := d.Open(context.Background(), root, cwdA, true)
	defer d.Close()
	for event := range events {
		d.ApplyLoad(event)
	}
	if !d.allScope || len(d.sessions) != 2 {
		t.Fatalf("all scope = %v, sessions = %#v", d.allScope, d.sessions)
	}
	if action := d.HandleKey(tui.Key{Kind: tui.KeyTab}); !action.ToggleScope {
		t.Fatalf("tab action = %#v, want scope toggle", action)
	}
}

func TestSessionDialogSearchShowsCountsAndExcerpt(t *testing.T) {
	d := newSessionDialog()
	d.active = true
	d.baseSessions = []core.SessionSummary{{Path: "one", MessageCount: 1}, {Path: "two", MessageCount: 1}}
	d.sessions = append([]core.SessionSummary(nil), d.baseSessions...)
	d.query = "alpha"
	d.searchReady = true
	d.searchGeneration = 1
	d.searchMatches = matchSessionSearchSegments(context.Background(), d.query, []core.SessionSearchSegment{
		{Path: "one", Text: "alpha implementation details"},
		{Path: "one", Text: "alpha follow-up"},
		{Path: "two", Text: "unrelated text"},
	})
	d.applySearchFilter()
	if len(d.sessions) != 1 || d.sessions[0].Path != "one" {
		t.Fatalf("search sessions = %#v", d.sessions)
	}
	match := d.searchMatches["one"]
	if match.count != 2 || match.excerpt == "" {
		t.Fatalf("search match = %#v", match)
	}
	rendered := stripANSIBytes(strings.Join(d.Render(tui.Dark, 120), "\n"))
	if !strings.Contains(rendered, "2 matches") || !strings.Contains(rendered, "alpha") {
		t.Fatalf("search render = %q", rendered)
	}
}

func TestSessionSearchUsesContiguousQueryAndRequiresTwoCharacters(t *testing.T) {
	segments := []core.SessionSearchSegment{
		{Path: "scattered", Text: "sizzle then harp, again remote"},
		{Path: "exact", Text: "resume the sharm project"},
	}

	if matches := matchSessionSearchSegments(context.Background(), "s", segments); matches != nil {
		t.Fatalf("one-character search = %#v, want no search", matches)
	}
	matches := matchSessionSearchSegments(context.Background(), "sharm", segments)
	if len(matches) != 1 {
		t.Fatalf("matches = %#v, want only exact session", matches)
	}
	match, ok := matches["exact"]
	if !ok {
		t.Fatalf("matches = %#v, missing exact session", matches)
	}
	if match.count != 1 || match.excerpt != "resume the sharm project" {
		t.Fatalf("exact match = %#v", match)
	}
	if want := []int{11, 12, 13, 14, 15}; len(match.indexes) != len(want) || match.indexes[0] != want[0] || match.indexes[1] != want[1] || match.indexes[2] != want[2] || match.indexes[3] != want[3] || match.indexes[4] != want[4] {
		t.Fatalf("highlight indexes = %v, want %v", match.indexes, want)
	}
}

func TestSessionDialogDefersSearchUntilSecondCharacter(t *testing.T) {
	d := newSessionDialog()
	d.active = true
	d.baseSessions = []core.SessionSummary{{Path: "one"}, {Path: "two"}}
	d.sessions = append([]core.SessionSummary(nil), d.baseSessions...)

	if action := d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: '/'}); action.StartSearch || !d.searchRequested {
		t.Fatalf("slash action = %#v, search requested = %v", action, d.searchRequested)
	}
	if action := d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: 's'}); action.StartSearch || d.query != "s" {
		t.Fatalf("one-character action = %#v, query = %q", action, d.query)
	}
	if len(d.sessions) != len(d.baseSessions) {
		t.Fatalf("one-character search filtered sessions: %#v", d.sessions)
	}
	if events := d.StartSearch(context.Background()); events != nil || d.searchEvents != nil {
		t.Fatal("one-character search started the corpus reader")
	}
	if action := d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: 'h'}); !action.StartSearch || d.query != "sh" {
		t.Fatalf("two-character action = %#v, query = %q", action, d.query)
	}
}

func TestSessionDialogClearsMatchesBelowSearchMinimum(t *testing.T) {
	d := newSessionDialog()
	d.active = true
	d.searchRequested = true
	d.searchReady = true
	d.query = "sh"
	d.baseSessions = []core.SessionSummary{{Path: "one", MessageCount: 1, FirstUserText: "session"}}
	d.sessions = append([]core.SessionSummary(nil), d.baseSessions...)
	d.searchMatches = map[string]sessionSearchMatch{"one": {count: 1}}

	d.HandleKey(tui.Key{Kind: tui.KeyBackspace})
	if d.searchMatches != nil {
		t.Fatalf("matches after shortening query = %#v, want nil", d.searchMatches)
	}
	rendered := stripANSIBytes(strings.Join(d.Render(tui.Dark, 120), "\n"))
	if strings.Contains(rendered, "1 match") {
		t.Fatalf("one-character search rendered stale match count: %q", rendered)
	}
}

func TestSessionDialogIgnoresStaleLoadResults(t *testing.T) {
	first := newSessionDialog()
	firstEvents := first.Open(context.Background(), t.TempDir(), t.TempDir())
	firstGeneration := first.loadGeneration

	secondEvents := first.Open(context.Background(), t.TempDir(), t.TempDir())
	secondGeneration := first.loadGeneration
	if secondGeneration == firstGeneration {
		t.Fatal("reopening picker did not advance load generation")
	}
	first.ApplyLoad(sessionLoadEvent{
		kind:       sessionLoadStarted,
		generation: secondGeneration,
		total:      1,
	})

	first.ApplyLoad(sessionLoadEvent{
		kind:       sessionLoadEntry,
		generation: firstGeneration,
		index:      0,
		summary: core.SessionSummary{
			Path:         "stale",
			MessageCount: 1,
		},
	})
	if len(first.sessions) != 0 {
		t.Fatalf("stale results repopulated picker: %+v", first.sessions)
	}

	first.Close()
	for range firstEvents {
	}
	for range secondEvents {
	}
}
