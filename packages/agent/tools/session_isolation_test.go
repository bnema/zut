package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func isolationText(result core.ToolResult) string {
	var b strings.Builder
	for _, c := range result.Content {
		if text, ok := c.(provider.TextBlock); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func TestPerSessionCWDIsolation(t *testing.T) {
	for _, marker := range []string{"session-one", "session-two"} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, marker+".txt"), []byte(marker), 0600); err != nil {
			t.Fatal(err)
		}
		t.Run(marker, func(t *testing.T) {
			t.Parallel()
			sb := NewSandbox(root)
			sb.Lock()
			cases := []struct {
				tool core.Tool
				args string
			}{
				{&ReadTool{CWD: root, Sandbox: sb}, `{"path":"` + marker + `.txt"}`},
				{&GlobTool{CWD: root, Sandbox: sb}, `{"pattern":"*.txt","path":"."}`},
				{&GrepTool{CWD: root, Sandbox: sb}, `{"pattern":"session-","path":"."}`},
			}
			for _, tc := range cases {
				result, err := tc.tool.Execute(context.Background(), json.RawMessage(tc.args), nil)
				if err != nil || result.IsError || !strings.Contains(isolationText(result), marker) {
					t.Fatalf("%s: result=%+v err=%v", tc.tool.Name(), result, err)
				}
			}
			if err := sb.CheckReadPath(marker + ".txt"); err != nil {
				t.Fatal(err)
			}
			if got := resolvePath(root, marker+".txt"); got != filepath.Join(root, marker+".txt") {
				t.Fatalf("resolved %q", got)
			}
			if runtime.GOOS != "windows" {
				bash := &BashTool{CWD: root, Env: []string{}}
				result, err := bash.Execute(context.Background(), json.RawMessage(`{"command":"pwd","timeout":5}`), nil)
				want, _ := filepath.EvalSymlinks(root)
				if err != nil || result.IsError || !strings.Contains(isolationText(result), want) {
					t.Fatalf("pwd: %+v %v", result, err)
				}
			}
		})
	}
}

func TestEnvIsolation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell syntax")
	}
	for _, value := range []string{"one", "two"} {
		root := t.TempDir()
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			env := []string{"X=" + value}
			tool := &BashTool{CWD: root, Env: env}
			result, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"printf '%s' \"$X\"","timeout":5}`), nil)
			if err != nil || result.IsError || !strings.Contains(isolationText(result), value) {
				t.Fatalf("env: %+v %v", result, err)
			}
			if got := (&pythonResolveDeps{Env: env}).getenv("X"); got != value {
				t.Fatalf("python X=%q", got)
			}
			if got := envValue(createWorktreeGitEnv(env), "X"); got != value {
				t.Fatalf("git X=%q", got)
			}
			child := pythonChildEnv(env)
			child[0] = "X=changed"
			if env[0] != "X="+value {
				t.Fatal("child environment aliases session env")
			}
		})
	}
}

func TestPerSessionMissingContext(t *testing.T) {
	for _, tool := range []core.Tool{&BashTool{}, &PythonTool{}, &ReadTool{}, &LSPTool{}, &CreateWorktreeTool{}, &ManageWorktreesTool{}} {
		_, err := tool.Execute(context.Background(), json.RawMessage(`{}`), nil)
		if err == nil || err.Error() != "tool "+tool.Name()+": session cwd not set" {
			t.Fatalf("%s: %v", tool.Name(), err)
		}
	}
	for _, tool := range []core.Tool{&BashTool{CWD: t.TempDir()}, &PythonTool{CWD: t.TempDir()}, &LSPTool{CWD: t.TempDir()}, &CreateWorktreeTool{CWD: t.TempDir()}, &ManageWorktreesTool{CWD: t.TempDir()}} {
		_, err := tool.Execute(context.Background(), json.RawMessage(`{}`), nil)
		if err == nil || err.Error() != "tool "+tool.Name()+": session env not set" {
			t.Fatalf("%s: %v", tool.Name(), err)
		}
	}
}
