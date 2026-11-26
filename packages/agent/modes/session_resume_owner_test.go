package modes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func awaitResumeResult(t *testing.T, i *Interactive) sessionResult {
	t.Helper()
	select {
	case res := <-i.sessionResults:
		if res.cancel != nil {
			t.Cleanup(res.cancel)
		}
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("resume did not complete")
		return sessionResult{}
	}
}

func TestResumeResolvesDaemonOwnerWithoutPicker(t *testing.T) {
	for _, scenario := range []string{"self", "symlink-alias"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			root := t.TempDir()
			realDir := filepath.Join(root, "real")
			require.NoError(t, os.Mkdir(realDir, 0700))
			path := filepath.Join(realDir, "session.jsonl")
			require.NoError(t, os.WriteFile(path, nil, 0600))
			pick := path
			if scenario == "symlink-alias" {
				alias := filepath.Join(root, "alias")
				require.NoError(t, os.Symlink(realDir, alias))
				pick = filepath.Join(alias, "session.jsonl")
			}
			owner := "other"
			if scenario == "self" {
				owner = "self"
			}
			infos := []session.Info{{ID: owner, SessionPath: path}}
			client := newMockSessionClient(t)
			i := &Interactive{client: client, sessionID: "self", portInfo: session.Info{CWD: root}, runCtx: ctx, sessionResults: make(chan sessionResult, 1), loopCalls: make(chan func(), 1)}
			client.EXPECT().List(mock.Anything).Return(infos, nil).Once()
			var port session.Port
			if scenario == "self" {
				// No expectations: any call, including Subscribe, fails the test.
				port = sessionmocks.NewMockPort(t)
				i.port = port
			} else {
				port = targetPort(t, owner)
				client.EXPECT().Attach(mock.Anything, session.ID(owner)).Return(port, nil).Once()
			}
			i.portResumeSession(pick, "")
			res := awaitResumeResult(t, i)
			require.NoError(t, res.err)
			require.Equal(t, session.ID(owner), res.id)
			if scenario == "self" {
				require.Equal(t, sessionResultKeepCurrent, res.kind)
				beforeGen := i.sessionGen
				events := make(chan session.Event)
				i.events = events
				canceled := make(chan struct{}, 1)
				i.subCancel = func() { canceled <- struct{}{} }
				require.False(t, i.applySessionResult(ctx, res))
				require.Equal(t, port, i.port)
				require.Equal(t, (<-chan session.Event)(events), i.events)
				require.Equal(t, beforeGen, i.sessionGen)
				require.Empty(t, canceled)
				require.Empty(t, i.statusOK)
				require.Empty(t, i.statusErr)
				client.AssertNotCalled(t, "Attach", mock.Anything, mock.Anything)
			} else {
				require.Equal(t, port, res.port)
			}
			client.AssertNotCalled(t, "Create", mock.Anything, mock.Anything)
		})
	}
}

func TestResumeListFailureDoesNotCreate(t *testing.T) {
	client := newMockSessionClient(t)
	client.EXPECT().List(mock.Anything).Return(nil, errors.New("list unavailable")).Once()
	i := &Interactive{client: client, sessionResults: make(chan sessionResult, 1)}
	i.portResumeSession("missing.jsonl", "")
	require.ErrorContains(t, awaitResumeResult(t, i).err, "list live sessions: list unavailable")
	client.AssertNotCalled(t, "Create", mock.Anything, mock.Anything)
}

func TestNormalizedSessionPathMissingFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.jsonl")
	require.Equal(t, path, normalizedSessionPath(path))
	cwd, cwdErr := os.Getwd()
	require.NoError(t, cwdErr)
	rel, err := filepath.Rel(cwd, path)
	require.NoError(t, err)
	require.Equal(t, path, normalizedSessionPath(rel))
}
