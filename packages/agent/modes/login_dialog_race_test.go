package modes

import (
	"context"
	"sync"
	"testing"

	"github.com/bnema/zut/packages/provider/auth"
	"github.com/bnema/zut/packages/tui"
)

func TestLoginDialogUpdatesShareRendererMutex(t *testing.T) {
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark})
	start := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		<-start
		for n := 0; n < 500; n++ {
			// Match the renderer's dialog read and editor-layout boundary.
			i.mu.Lock()
			if i.dialog.Active() {
				i.dialog.Render(tui.Dark, 80)
				i.dialog.CursorPos(80)
			}
			i.mu.Unlock()
		}
	}()
	close(start)
	for n := 0; n < 500; n++ {
		i.mu.Lock()
		i.dialog.step = loginStepWaiting
		i.dialog.url = ""
		i.mu.Unlock()
		i.showLoginAction(false, "https://example.test/login")
		i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: 'x'})
		i.handleAuthEvent(auth.Event{Kind: "error", Message: "synthetic login failure"})
		i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	}
	readers.Wait()
}
