package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestOpenOrCreateSessionStateFreshness(t *testing.T) {
	for _, name := range []string{"new", "no-session", "new-explicit", "existing-explicit", "continue-existing", "resume-id", "continue-missing"} {
		t.Run(name, func(t *testing.T) {
			home, cwd := t.TempDir(), t.TempDir()
			t.Setenv("ZUT_HOME", home)
			args := Args{CWD: cwd}
			resolved := Resolved{CWD: cwd, Provider: "test-provider", Model: "test-model"}
			wantFresh := true
			switch name {
			case "no-session":
				args.NoSess = true
			case "new-explicit":
				args.Session = filepath.Join(home, "explicit.jsonl")
			case "existing-explicit", "continue-existing", "resume-id":
				old, err := core.NewSession(home, cwd, resolved.Provider, resolved.Model, "test")
				if err != nil {
					t.Fatal(err)
				}
				if name != "existing-explicit" {
					if err := old.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "saved prompt"}}}); err != nil {
						t.Fatal(err)
					}
				}
				path, id := old.Path, old.ID
				if name == "existing-explicit" {
					// Keep the empty session open so its valid header remains;
					// restoration is not inferred from message count.
					t.Cleanup(func() { _ = old.Close() })
					args.Session = path
				} else {
					if err := old.Close(); err != nil {
						t.Fatal(err)
					}
					if name == "continue-existing" {
						args.Continue = true
					} else {
						args.Resume, args.ResumeSessionID = true, id
					}
				}
				wantFresh = false
			case "continue-missing":
				args.Continue = true
			}
			ag := core.NewAgent(nil, resolved.Model, "system", core.Registry{})
			sess, fresh, err := openOrCreateSessionState(context.Background(), args, resolved, ag, "test")
			if err != nil {
				t.Fatal(err)
			}
			if sess != nil {
				defer sess.Close()
			}
			if fresh != wantFresh {
				t.Fatalf("fresh=%v, want %v", fresh, wantFresh)
			}
			if args.NoSess && sess != nil {
				t.Fatal("session created with persistence disabled")
			}
		})
	}
}
