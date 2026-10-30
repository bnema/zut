package modes

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
)

func TestResidentSubagentListUsesHumanLabelTerminalStateAndRelativeTime(t *testing.T) {
	row := subagents.ResidentSnapshot{ID: "b6382ac7-ca36-45f1-9549-dd9ee4d195b7", Profile: "go-reviewer", Model: "gpt-5.6-luna", State: subagents.ResidentIdle}
	if got := residentDisplayName(row); got != "go-reviewer" {
		t.Fatalf("display name = %q", got)
	}
	if got := residentDisplayState(row.State); got != "completed" {
		t.Fatalf("display state = %q", got)
	}
	if got := shortResidentID(row.ID); got != "b6382ac7" {
		t.Fatalf("short ID = %q", got)
	}
	if got := formatResidentUpdatedAt(time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC), time.Date(2026, time.August, 13, 12, 2, 0, 0, time.UTC)); got != "2m ago" {
		t.Fatalf("relative update = %q", got)
	}
}

func TestResidentSubagentsDialogShowsUsageMetadata(t *testing.T) {
	usage := provider.Usage{InputTokens: 84_000, OutputTokens: 1_500, CacheReadTokens: 123_000, CacheMeasuredPromptTokens: 207_000, CacheMeasuredReadTokens: 123_000, CostUSD: 0.525}
	rows := make([]subagents.ResidentSnapshot, 2)
	for index := range rows {
		rows[index] = subagents.ResidentSnapshot{ID: fmt.Sprintf("usage-child-%d", index), State: subagents.ResidentIdle, Profile: "reviewer", Provider: "openai-codex", Model: "gpt-test", Usage: usage, ContextUsed: 207_000, ContextMax: 272_000, Subscription: true}
	}
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().ResidentList(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, offset, limit int) ([]subagents.ResidentSnapshot, int, error) {
		end := min(len(rows), offset+limit)
		if offset >= end {
			return nil, len(rows), nil
		}
		return rows[offset:end], len(rows), nil
	})
	dialog, loaded := newTestResidentDialog()
	openResidentDialog(t, dialog, port, loaded)
	plain := strings.Join(plainResidentIndicatorLines(renderDialog(dialog, 100, 10)), "\n")
	if !strings.Contains(plain, "\n    ↑84k ↓1.5k R123k/ C59.4% $0.525 (sub) 76.1%/272k") {
		t.Fatalf("dialog = %q, want usage metadata line", plain)
	}
	for _, height := range []int{3, 4} {
		if lines := renderDialog(dialog, 100, height); len(lines) > height {
			t.Fatalf("height %d rendered %d lines: %q", height, len(lines), plainResidentIndicatorLines(lines))
		}
	}
	for width := 1; width <= 3; width++ {
		for _, line := range plainResidentIndicatorLines(renderDialog(dialog, width, 10)) {
			if line == "    " {
				t.Fatalf("width %d rendered an empty metadata row", width)
			}
		}
	}
}

func TestResidentSubagentsDialogBoundsEmptyStateByHeight(t *testing.T) {
	dialog, _ := newTestResidentDialog()
	dialog.lock.Lock()
	dialog.Open(nil)
	dialog.lock.Unlock()
	tests := []struct {
		height int
		want   []string
	}{
		{height: 0, want: nil},
		{height: 1, want: []string{"  Resident subagents"}},
		{height: 2, want: []string{"  Resident subagents", "  Enter: open   Esc: close   /subagents new <task>: spawn"}},
		{height: 3, want: []string{"  Resident subagents", "  Enter: open   Esc: close   /subagents new <task>: spawn", "  No resident subagents."}},
	}
	for _, test := range tests {
		if got := plainResidentIndicatorLines(renderDialog(dialog, 100, test.height)); !slices.Equal(got, test.want) {
			t.Errorf("height %d rendered %q, want %q", test.height, got, test.want)
		}
	}
}

func TestResidentSubagentsDialogNilReceiver(t *testing.T) {
	var dialog *residentSubagentsDialog
	dialog.Invalidate()
	dialog.Close()
}

// newTestResidentDialog returns a dialog whose fetch completions are signalled
// on loaded, so tests synchronize without sleeping.
func newTestResidentDialog() (*residentSubagentsDialog, chan struct{}) {
	loaded := make(chan struct{}, 8)
	notify := func() {
		select {
		case loaded <- struct{}{}:
		default:
		}
	}
	return newResidentSubagentsDialog(&sync.Mutex{}, notify), loaded
}

// renderDialog renders under the owner lock, as Interactive.redraw does.
func renderDialog(dialog *residentSubagentsDialog, width, height int) []string {
	dialog.lock.Lock()
	defer dialog.lock.Unlock()
	return dialog.Render(tui.Dark, width, height)
}

func waitDialogFetch(t *testing.T, loaded <-chan struct{}) {
	t.Helper()
	select {
	case <-loaded:
	case <-time.After(5 * time.Second):
		t.Fatal("resident list fetch did not complete")
	}
}

func openResidentDialog(t *testing.T, dialog *residentSubagentsDialog, port session.Port, loaded <-chan struct{}) {
	t.Helper()
	dialog.lock.Lock()
	dialog.Open(port)
	dialog.lock.Unlock()
	waitDialogFetch(t, loaded)
}

func TestResidentSubagentsDialogListsFromPortAndSelects(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	rows := []subagents.ResidentSnapshot{{ID: "first", State: subagents.ResidentRunning, Model: "m"}, {ID: "second", State: subagents.ResidentIdle, Model: "m"}}
	port.EXPECT().ResidentList(mock.Anything, 0, residentDialogInitialLimit).Return(rows, 2, nil).Once()
	dialog, loaded := newTestResidentDialog()
	openResidentDialog(t, dialog, port, loaded)
	dialog.lock.Lock()
	defer dialog.lock.Unlock()
	dialog.HandleKey(tui.Key{Kind: tui.KeyDown})
	if got := dialog.HandleKey(tui.Key{Kind: tui.KeyEnter}); got != "second" {
		t.Fatalf("selected = %q, want second", got)
	}
}

func TestResidentSubagentsDialogShowsPortError(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().ResidentList(mock.Anything, mock.Anything, mock.Anything).Return(nil, 0, errors.New("daemon gone")).Once()
	dialog, loaded := newTestResidentDialog()
	openResidentDialog(t, dialog, port, loaded)
	plain := strings.Join(plainResidentIndicatorLines(renderDialog(dialog, 100, 2*residentDialogInitialLimit+3)), "\n")
	if !strings.Contains(plain, "unavailable: daemon gone") || strings.Contains(plain, "No resident subagents") {
		t.Fatalf("dialog = %q, want port error", plain)
	}
}

// Render must never call the port: it shows "Loading" until the async fetch
// lands, and steady renders reuse the cached window.
func TestResidentSubagentsDialogRenderReadsCacheOnly(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	release := make(chan struct{})
	port.EXPECT().ResidentList(mock.Anything, 0, 3).RunAndReturn(func(context.Context, int, int) ([]subagents.ResidentSnapshot, int, error) {
		<-release
		return []subagents.ResidentSnapshot{{ID: "a", Model: "m"}}, 1, nil
	}).Once()
	dialog, loaded := newTestResidentDialog()
	dialog.lock.Lock()
	// Open without the initial fetch so the render below sizes the window.
	dialog.active, dialog.port = true, port
	if lines := plainResidentIndicatorLines(dialog.Render(tui.Dark, 100, 9)); !containsLine(lines, "Loading") {
		t.Fatalf("render before fetch = %q, want loading", lines)
	}
	dialog.lock.Unlock()
	close(release)
	waitDialogFetch(t, loaded)
	dialog.lock.Lock()
	defer dialog.lock.Unlock()
	for range 3 {
		if lines := plainResidentIndicatorLines(dialog.Render(tui.Dark, 100, 9)); !containsLine(lines, " a") {
			t.Fatalf("render = %q", lines)
		}
	}
}

// signalLocker reports every Unlock so a test can wait until another
// goroutine has finished a critical section.
type signalLocker struct {
	sync.Mutex
	unlocked chan struct{}
}

func (l *signalLocker) Unlock() {
	l.Mutex.Unlock()
	select {
	case l.unlocked <- struct{}{}:
	default:
	}
}

// A fetch that completes after Close must not publish into the dialog. The
// fetch goroutine takes the dialog lock exactly once after the port returns,
// either to discard (stale generation) or to apply; the test waits for that
// unlock before inspecting, so the check cannot pass vacuously.
func TestResidentSubagentsDialogCloseDiscardsInFlightFetch(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	started, release := make(chan struct{}), make(chan struct{})
	port.EXPECT().ResidentList(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(context.Context, int, int) ([]subagents.ResidentSnapshot, int, error) {
		close(started)
		<-release
		return []subagents.ResidentSnapshot{{ID: "late"}}, 1, nil
	}).Once()
	lock := &signalLocker{unlocked: make(chan struct{}, 1)}
	notified := make(chan struct{}, 1)
	dialog := newResidentSubagentsDialog(lock, func() { notified <- struct{}{} })
	lock.Lock()
	dialog.Open(port)
	lock.Unlock()
	<-started
	lock.Lock()
	dialog.Close()
	lock.Unlock()
	<-lock.unlocked // drain the test's own unlocks; capacity 1 keeps the last
	close(release)
	select {
	case <-lock.unlocked: // the fetch goroutine's single critical section
	case <-time.After(5 * time.Second):
		t.Fatal("fetch goroutine never took the dialog lock")
	}
	// Bypass signalLocker.Unlock so this read sends no signal.
	lock.Lock()
	gotRows, gotTotal, gotLoaded := dialog.rows, dialog.total, dialog.loaded
	lock.Mutex.Unlock()
	if gotLoaded || gotTotal != 0 || len(gotRows) != 0 {
		t.Fatalf("closed dialog applied a stale fetch: rows=%v total=%d loaded=%v", gotRows, gotTotal, gotLoaded)
	}
	select {
	case <-notified:
		t.Fatal("closed dialog published a stale fetch")
	default:
	}
}

func TestResidentSubagentsDialogInvalidateRefetches(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().ResidentList(mock.Anything, 0, residentDialogInitialLimit).Return(nil, 0, nil).Once()
	port.EXPECT().ResidentList(mock.Anything, 0, residentDialogInitialLimit).Return([]subagents.ResidentSnapshot{{ID: "new"}}, 1, nil).Once()
	dialog, loaded := newTestResidentDialog()
	openResidentDialog(t, dialog, port, loaded)
	dialog.lock.Lock()
	dialog.Invalidate()
	dialog.lock.Unlock()
	waitDialogFetch(t, loaded)
	dialog.lock.Lock()
	defer dialog.lock.Unlock()
	if dialog.total != 1 || len(dialog.rows) != 1 {
		t.Fatalf("rows/total = %v/%d after invalidate", dialog.rows, dialog.total)
	}
}

// While the next window is in flight the cached rows still belong to the old
// window; Enter must resolve against them, never misindex into them.
func TestResidentSubagentsDialogEnterDuringPagingFetch(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	rows := []subagents.ResidentSnapshot{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	port.EXPECT().ResidentList(mock.Anything, 0, 2).Return(rows[:2], 3, nil).Once()
	release := make(chan struct{})
	port.EXPECT().ResidentList(mock.Anything, 1, 2).RunAndReturn(func(context.Context, int, int) ([]subagents.ResidentSnapshot, int, error) {
		<-release
		return rows[1:], 3, nil
	}).Once()
	dialog, loaded := newTestResidentDialog()
	dialog.lock.Lock()
	dialog.active, dialog.port, dialog.limit = true, port, 2
	dialog.schedule()
	dialog.lock.Unlock()
	waitDialogFetch(t, loaded)

	dialog.lock.Lock()
	dialog.HandleKey(tui.Key{Kind: tui.KeyDown})
	dialog.HandleKey(tui.Key{Kind: tui.KeyDown}) // selects "c": window moves to offset 1
	if got := dialog.HandleKey(tui.Key{Kind: tui.KeyEnter}); got != "" {
		t.Fatalf("Enter during paging fetch opened %q, want none", got)
	}
	dialog.lock.Unlock()

	close(release)
	waitDialogFetch(t, loaded)
	dialog.lock.Lock()
	defer dialog.lock.Unlock()
	if got := dialog.HandleKey(tui.Key{Kind: tui.KeyEnter}); got != "c" {
		t.Fatalf("Enter after fetch opened %q, want c", got)
	}
}
