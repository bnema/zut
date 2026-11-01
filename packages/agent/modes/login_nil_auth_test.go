package modes

import "testing"

// A TUI without an auth manager must report login as unavailable instead of
// dereferencing a nil manager.
func TestLoginFlowsWithoutAuthManagerDoNotPanic(t *testing.T) {
	i := NewInteractive(InteractiveConfig{Terminal: &alertTestTerminal{}})
	i.startOAuthFlow("openai-codex")
	i.startOAuthFlow("github-copilot")
	i.startAPIKeyFlow("openai")
}
