package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
)

func TestResidentStatusResultReadErrors(t *testing.T) {
	for _, scenario := range []struct {
		name string
		want string
	}{
		{"missing", "no saved result exists yet"},
		{"foreign", "owned by another zut process"},
		{"corrupt", "could not read or decode the saved result"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			manager := subagents.NewResidentManager(root, func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
				return func(ctx context.Context, _ string) error {
					<-ctx.Done()
					return ctx.Err()
				}, nil
			})
			t.Cleanup(func() { _ = manager.Close(context.Background()) })
			spec := subagents.ResidentChildSpec{ID: "status-error", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test"}
			if _, err := manager.Spawn(t.Context(), spec, "investigate"); err != nil {
				t.Fatal(err)
			}
			statusManager := manager
			if scenario.name == "foreign" {
				statusManager = subagents.NewResidentManager(root, nil)
				t.Cleanup(func() { _ = statusManager.Close(context.Background()) })
				if errs := statusManager.Reconcile(); len(errs) != 0 {
					t.Fatal(errs)
				}
			}
			if scenario.name == "corrupt" {
				if err := os.WriteFile(filepath.Join(root, spec.ID, "result.json"), []byte("private malformed payload"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tool := &SubagentStatusTool{ResidentManager: statusManager, Enabled: func() bool { return true }}
			result, err := tool.Execute(t.Context(), json.RawMessage(`{"agent_id":"status-error","include_result":true}`), nil)
			if err != nil || !result.IsError {
				t.Fatalf("result = %#v, error = %v", result, err)
			}
			text := toolResultText(t, result)
			if !strings.Contains(text, scenario.want) || strings.Contains(text, root) || strings.Contains(text, "private") {
				t.Fatalf("unexpected error text: %q", text)
			}
		})
	}
}
