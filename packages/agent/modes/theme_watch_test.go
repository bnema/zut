package modes

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bnema/zut/packages/tui"
)

func writeWatchedTheme(t *testing.T, home, body string) string {
	t.Helper()
	path := filepath.Join(home, "themes", "live.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func advanceThemeWatch(t *testing.T, ticks chan<- time.Time, processed <-chan struct{}, now time.Time) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	select {
	case ticks <- now:
	case <-deadline.C:
		t.Fatal("watcher did not accept tick")
	}
	select {
	case <-processed:
	case <-deadline.C:
		t.Fatal("watcher did not process tick")
	}
}

func TestWatchThemeSourceTicksPublishesOnlyValidRevisions(t *testing.T) {
	home := t.TempDir()
	path := writeWatchedTheme(t, home, `{"colors":{"accent":20}}`)
	accepted, err := tui.LoadThemeSource(home, "live")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	processed := make(chan struct{})
	events := make(chan themeWatchEvent, 3)
	go watchThemeSourceTicks(ctx, home, "live", accepted, ticks, events, processed)

	if err := os.WriteFile(path, []byte(`{"colors":{"accent":21}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	advanceThemeWatch(t, ticks, processed, time.Unix(1, 0))
	select {
	case event := <-events:
		if event.source == nil || event.source.File.Colors.Base.Accent == nil || event.source.File.Colors.Base.Accent.Index != 21 {
			t.Fatalf("valid revision event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher did not publish valid revision")
	}

	if err := os.WriteFile(path, []byte(`{"colors":{"accent":999}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	advanceThemeWatch(t, ticks, processed, time.Unix(2, 0))
	select {
	case event := <-events:
		if event.err == nil || event.source != nil {
			t.Fatalf("invalid revision event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher did not report invalid revision")
	}
}

func TestWatchThemeSourceTicksRestartsDeletionGraceAfterInvalidReappearance(t *testing.T) {
	home := t.TempDir()
	path := writeWatchedTheme(t, home, `{}`)
	accepted, err := tui.LoadThemeSource(home, "live")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	processed := make(chan struct{})
	events := make(chan themeWatchEvent, 1)
	go watchThemeSourceTicks(ctx, home, "live", accepted, ticks, events, processed)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	advanceThemeWatch(t, ticks, processed, time.Unix(1, 0))
	if err := os.WriteFile(path, []byte(`{"colors":{"accent":999}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	advanceThemeWatch(t, ticks, processed, time.Unix(2, 0))
	select {
	case event := <-events:
		if event.err == nil {
			t.Fatalf("invalid reappearance event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher did not report invalid reappearance")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	advanceThemeWatch(t, ticks, processed, time.Unix(3, 0))
	advanceThemeWatch(t, ticks, processed, time.Unix(4, 0))
	select {
	case event := <-events:
		t.Fatalf("deletion happened before restarted grace period: %#v", event)
	default:
	}
	advanceThemeWatch(t, ticks, processed, time.Unix(6, 0))
	select {
	case event := <-events:
		if !event.deleted {
			t.Fatalf("persistent deletion event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher did not publish persistent deletion")
	}
}
