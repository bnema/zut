package modes

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Without Run: refreshDictation is driven directly with a queued result.
func TestInteractivePortDictationSubmitsTranscript(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	called := make(chan struct{})
	var got session.Input
	port.EXPECT().Submit(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in session.Input) error {
		got = in
		close(called)
		return nil
	}).Once()
	i := NewInteractive(InteractiveConfig{Terminal: &alertTestTerminal{}, Port: port})
	events := make(chan dictationEvent, 1)
	done := make(chan struct{})
	close(done)
	events <- dictationEvent{text: " hello world "}
	i.mu.Lock()
	i.dictation = interactiveDictation{phase: "transcribing", events: events, cancel: func() {}, done: done}
	i.mu.Unlock()
	i.refreshDictation()
	waitSignal(t, called, "Submit")
	require.Equal(t, "hello world", got.Text)
	require.Equal(t, session.InputOriginUser, got.Origin)
	i.mu.Lock()
	defer i.mu.Unlock()
	require.Equal(t, "", strings.Join(i.ed.Lines, ""))
}

func startPortTelegram(t *testing.T) (*mockSessionClient, *portRun) {
	t.Helper()
	client := newMockSessionClient(t)
	r := startPortInteractive(t, newCmdPort(t, "/w"), client)
	r.waitOutput(t, "")
	return client, r
}

func TestInteractivePortTelegramConnect(t *testing.T) {
	client, r := startPortTelegram(t)
	called := make(chan struct{})
	client.EXPECT().TelegramBridge(mock.Anything, "s1", true).RunAndReturn(func(context.Context, string, bool) (TelegramBridgeStatus, error) {
		close(called)
		return TelegramBridgeStatus{Active: true, Session: "s1", Username: "zbot", PairedUserID: 7}, nil
	}).Once()
	requireEventuallySessionID(t, r)
	r.i.doTelegram("connect")
	waitSignal(t, called, "TelegramBridge")
	r.waitOutput(t, "telegram connected as @zbot")
}

func TestInteractivePortTelegramDisconnect(t *testing.T) {
	client, r := startPortTelegram(t)
	called := make(chan struct{})
	client.EXPECT().TelegramBridge(mock.Anything, "s1", false).RunAndReturn(func(context.Context, string, bool) (TelegramBridgeStatus, error) {
		close(called)
		return TelegramBridgeStatus{}, nil
	}).Once()
	requireEventuallySessionID(t, r)
	r.i.doTelegram("disconnect")
	waitSignal(t, called, "TelegramBridge")
	r.waitOutput(t, "telegram disconnected")
}

func TestInteractivePortTelegramErrorShown(t *testing.T) {
	client, r := startPortTelegram(t)
	client.EXPECT().TelegramBridge(mock.Anything, "s1", true).Return(TelegramBridgeStatus{}, errors.New("bot is already running")).Once()
	requireEventuallySessionID(t, r)
	r.i.doTelegram("connect")
	r.waitOutput(t, "telegram connect: bot is already running")
}

func requireEventuallySessionID(t *testing.T, r *portRun) {
	t.Helper()
	require.Eventually(t, func() bool { return r.i.SessionID() == "s1" }, 5e9, 1e7)
}
