package modes

import "testing"

// A TUI without an auth manager must report login as unavailable instead of
// dereferencing a nil manager.
func TestLoginFlowsWithoutAuthManagerReportUnavailable(t *testing.T) {
	for _, tt := range []struct {
		name  string
		start func(*Interactive)
	}{
		{"oauth browser", func(i *Interactive) { i.startOAuthFlow("openai-codex") }},
		{"oauth device code", func(i *Interactive) { i.startOAuthFlow("github-copilot") }},
		{"api key", func(i *Interactive) { i.startAPIKeyFlow("openai") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			i := NewInteractive(InteractiveConfig{Terminal: &alertTestTerminal{}, ZutHome: t.TempDir()})
			i.dialog.Open(t.TempDir())
			tt.start(i)
			if i.dialog.step != loginStepDone || i.dialog.success {
				t.Fatalf("dialog step=%v success=%v, want failed result", i.dialog.step, i.dialog.success)
			}
			if want := "login is unavailable: auth manager not configured"; i.dialog.message != want {
				t.Fatalf("message=%q, want %q", i.dialog.message, want)
			}
		})
	}
}
