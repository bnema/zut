package modes

import "context"

// Submit feeds text through the agent loop as if the user had typed it.
func (i *Interactive) Submit(text string) {
	if cmd, ok := shellEscapeCommand(text); ok {
		i.startShellEscape(i.runCtx, cmd)
		return
	}
	parent := i.runCtx
	if parent == nil {
		parent = context.Background()
	}
	i.mu.Lock()
	awaitingStartupPre := i.awaitingStartupPre
	i.mu.Unlock()
	if !awaitingStartupPre {
		i.maybeStartSessionTitle(parent, text)
	}
	i.startTurn(parent, text)
}
