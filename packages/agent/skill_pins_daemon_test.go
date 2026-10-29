package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/require"
)

// TestDaemonHeadlessSessionPreloadsSkillPinsOnce covers the headless (print,
// stream, json) daemon path: a fresh session folds pinned skill bodies into
// its first prompt, a resumed one does not, and the body persists once.
func TestDaemonHeadlessSessionPreloadsSkillPinsOnce(t *testing.T) {
	home := sessionAssemblyEnv(t)
	t.Setenv("ZUT_AGENT_SKILLS", "")
	t.Setenv("ZUT_AGENT_PROFILES", "")
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".zut", "skills", "review")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: review\ndescription: Review things.\n---\nPINNED_INSTRUCTIONS\n"), 0o600))
	require.NoError(t, toggleSkillPin(cwd, "review", true))

	requests := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		for i := len(request.Messages) - 1; i >= 0; i-- {
			if request.Messages[i].Role == "user" {
				requests <- string(request.Messages[i].Content)
				break
			}
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	td := startFactoryDaemon(t, home, daemonTestSocket(t))
	c := dialTestDaemon(t, td.socket)
	ctx := factoryTestCtx(t)
	env := []string{"PATH=" + os.Getenv("PATH")}
	args := append(sessionAssemblyArgs(server), "--with-skills")

	first, err := c.Create(ctx, session.CreateSpec{CWD: cwd, Env: env, Args: args, Headless: true})
	require.NoError(t, err)
	require.Equal(t, "done", submitAndWait(t, ctx, c, session.ID(first.ID)))
	select {
	case prompt := <-requests:
		require.Contains(t, prompt, "PINNED_INSTRUCTIONS", "fresh headless prompt lacks pinned body")
	default:
		t.Fatal("no provider request")
	}
	require.NotEmpty(t, first.SessionPath)
	require.NoError(t, c.Close(ctx, session.ID(first.ID), "test"))

	resumed, err := c.Create(ctx, session.CreateSpec{CWD: cwd, Env: env, Args: args, Headless: true, ResumePath: first.SessionPath})
	require.NoError(t, err)
	require.Equal(t, "done", submitAndWait(t, ctx, c, session.ID(resumed.ID)))
	select {
	case prompt := <-requests:
		require.NotContains(t, prompt, "PINNED_INSTRUCTIONS", "resumed prompt reloaded pins")
	default:
		t.Fatal("no resumed provider request")
	}
	require.NoError(t, c.Close(ctx, session.ID(resumed.ID), "test"))

	sess, messages, err := core.OpenSession(first.SessionPath)
	require.NoError(t, err)
	defer sess.Close()
	count := 0
	for _, m := range messages {
		if m.Role != provider.RoleUser {
			continue
		}
		for _, content := range m.Content {
			if text, ok := content.(provider.TextBlock); ok {
				count += strings.Count(text.Text, "PINNED_INSTRUCTIONS")
			}
		}
	}
	require.Equal(t, 1, count, "persisted pinned bodies")
}
