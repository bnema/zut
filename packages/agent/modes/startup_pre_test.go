package modes

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
)

func applyStartupPreResultForTest(t *testing.T, i *Interactive) {
	t.Helper()
	select {
	case result := <-i.startupPreDone:
		i.applyStartupPreResult(result)
	default:
		t.Fatal("startup pre result was not queued")
	}
}

func TestCompleteStartupPrePrefillsEditor(t *testing.T) {
	i := NewInteractive(InteractiveConfig{})
	i.awaitingStartupPre = true
	i.deferredInitialInput = "Bom dia!"
	i.autoSubmitDeferred = false
	i.completeStartupPre()
	applyStartupPreResultForTest(t, i)
	if got := i.ed.Value(); got != "Bom dia!" {
		t.Fatalf("editor = %q, want Bom dia!", got)
	}
	i.mu.Lock()
	awaiting := i.awaitingStartupPre
	i.mu.Unlock()
	if awaiting {
		t.Fatal("awaitingStartupPre still set after completeStartupPre")
	}
}

func TestStartupPrePrefillPreservesTypedInput(t *testing.T) {
	i := NewInteractive(InteractiveConfig{})
	i.ed.SetValue("typed while reloading")
	i.applyStartupPreResult(startupPreResult{deferred: "default prompt"})
	if got := i.ed.Value(); got != "typed while reloading" {
		t.Fatalf("editor = %q, want typed input to be preserved", got)
	}
}

func TestCompleteStartupPreCallsOnDone(t *testing.T) {
	called := false
	i := NewInteractive(InteractiveConfig{
		OnStartupPreDone: func() { called = true },
	})
	i.awaitingStartupPre = true
	i.completeStartupPre()
	if !called {
		t.Fatal("OnStartupPreDone was not called")
	}
}

func TestStartupPreShellWithoutAgentThenPrefill(t *testing.T) {
	i := NewInteractive(InteractiveConfig{CWD: t.TempDir()})
	i.runCtx = context.Background()
	i.awaitingStartupPre = true
	i.deferredInitialInput = "after-pre"
	i.autoSubmitDeferred = false

	cmd := "printf zut-pre"
	if runtime.GOOS == "windows" {
		cmd = "echo zut-pre"
	}
	i.startShellEscape(context.Background(), cmd)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case result := <-i.startupPreDone:
			i.applyStartupPreResult(result)
		default:
		}
		i.mu.Lock()
		running := i.shellRunning
		awaiting := i.awaitingStartupPre
		i.mu.Unlock()
		if !running && !awaiting {
			if got := i.ed.Value(); got != "after-pre" {
				t.Fatalf("editor = %q, want after-pre", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("startup pre did not complete and prefill the editor")
}

func TestStartupPreShellThenAutoSubmitShell(t *testing.T) {
	i, port := newPortShellInteractive(t)
	sent := make(chan string, 2)
	port.EXPECT().AppendShellContext(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, text string) error {
		sent <- text
		return nil
	}).Times(2)
	i.runCtx = context.Background()
	i.awaitingStartupPre = true
	i.deferredInitialInput = "!printf zut-default"
	if runtime.GOOS == "windows" {
		i.deferredInitialInput = "!echo zut-default"
	}
	i.autoSubmitDeferred = true

	cmd := "printf zut-pre"
	if runtime.GOOS == "windows" {
		cmd = "echo zut-pre"
	}
	i.startShellEscape(context.Background(), cmd)

	var got []string
	deadline := time.After(5 * time.Second)
	for len(got) < 2 {
		select {
		case result := <-i.startupPreDone:
			i.applyStartupPreResult(result)
		case text := <-sent:
			got = append(got, text)
		case <-deadline:
			t.Fatalf("startup pre did not auto-submit the deferred prompt; sent %q", got)
		}
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "zut-pre") || !strings.Contains(joined, "zut-default") {
		t.Fatalf("context = %q, want both pre and default shell output", joined)
	}
}
