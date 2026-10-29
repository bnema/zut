package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/modes/telegram"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const testBotToken = "123456:SECRET-test-token"

// tgServer is an httptest Telegram Bot API: getUpdates hands out queued
// updates, sendMessage records outgoing texts.
type tgServer struct {
	srv     *httptest.Server
	updates chan telegram.Update
	sent    chan string
}

func newTGServer(t *testing.T) *tgServer {
	s := &tgServer{updates: make(chan telegram.Update, 8), sent: make(chan string, 16)}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *tgServer) serve(w http.ResponseWriter, r *http.Request) {
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+testBotToken+"/")
	if !ok {
		http.Error(w, `{"ok":false,"description":"bad token"}`, http.StatusUnauthorized)
		return
	}
	reply := func(result any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}
	switch method {
	case "getMe":
		reply(telegram.User{ID: 99, IsBot: true, Username: "zutbot"})
	case "deleteWebhook":
		reply(true)
	case "getUpdates":
		select {
		case u := <-s.updates:
			reply([]telegram.Update{u})
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
			reply([]telegram.Update{})
		}
	case "sendMessage":
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.sent <- body.Text
		reply(map[string]any{"message_id": 1})
	default:
		reply(true)
	}
}

var nextUpdate int64

func (s *tgServer) dm(text string) {
	nextUpdate++
	s.updates <- telegram.Update{UpdateID: nextUpdate, Message: &telegram.Message{
		MessageID: int(nextUpdate), From: &telegram.User{ID: 7, Username: "owner"},
		Chat: telegram.Chat{ID: 7, Type: "private"}, Text: text,
	}}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// botTest is a real daemon whose generated BotFactory mock delegates bot
// work to a real telegram.Service talking to the httptest API.
type botTest struct {
	r    *running
	f    *mockBotFactory
	tg   *tgServer
	logs *syncBuffer
	c    *testClient
	home string
}

func newBotTest(t *testing.T, sessions ...*testSession) *botTest {
	home := t.TempDir()
	tg := newTGServer(t)
	if err := telegram.SaveConfig(home, telegram.Config{BotToken: testBotToken, AllowedUserID: 7, APIBase: tg.srv.URL}); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	svc := &telegram.Service{Home: home, Log: logs}
	f := newMockBotFactory(t)
	for _, s := range sessions {
		f.EXPECT().Create(mock.Anything, mock.Anything).Return(s.h, nil).Once()
		s.h.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{}, nil).Maybe()
		s.h.EXPECT().ApplySettings(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, p session.SettingsPatch) error {
			if p.WebSearchEnabled == nil || *p.WebSearchEnabled {
				t.Errorf("bot must disable web search, patch = %+v", p)
			}
			return nil
		}).Maybe()
	}
	f.EXPECT().CheckBot(mock.Anything).RunAndReturn(func(string) error { return svc.Check() }).Maybe()
	f.EXPECT().BotIdentity(mock.Anything).RunAndReturn(func(string) (string, int64) { return svc.Identity() }).Maybe()
	f.EXPECT().RunBot(mock.Anything, "telegram", mock.Anything).RunAndReturn(func(ctx context.Context, _ string, p session.Port) error {
		return svc.RunBot(ctx, p)
	}).Maybe()
	f.EXPECT().RunBridge(mock.Anything, "telegram", mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, _ string, p session.Port, ready func(error)) error {
		return svc.RunBridge(ctx, p, ready)
	}).Maybe()
	r := startDaemon(t, f, home, Options{Logger: slog.New(slog.NewTextHandler(logs, nil))})
	return &botTest{r: r, f: f, tg: tg, logs: logs, c: dial(t, r.sock), home: home}
}

// waitSubs waits until n subscriptions (daemon watcher + bot) exist.
func waitSubs(t *testing.T, s *testSession, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		got := len(s.subs)
		s.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("subscriptions < %d", n)
}

func turn(s *testSession, seq uint64, origin session.InputOrigin, user, answer string) {
	s.emit(session.Event{Seq: seq, Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{Run: session.RunStateRunning, CurrentOrigin: origin}}})
	if user != "" {
		s.emit(session.Event{Seq: seq + 1, Kind: session.EvAgent, Agent: core.EvUserMessage{Message: provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: user}}}}})
	}
	s.emit(session.Event{Seq: seq + 2, Kind: session.EvAgent, Agent: core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: answer}}}}})
	s.emit(session.Event{Seq: seq + 3, Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{Run: session.RunStateIdle}}})
}

func TestBotStartStopStatusThroughDaemon(t *testing.T) {
	s1 := newTestSession(t, "s1")
	bt := newBotTest(t, s1)

	var st daemonproto.BotStatusData
	if err := bt.c.call(t, daemonproto.BotStatusRequest{Type: "bot_status", Kind: "telegram"}, &st); err != nil || st.Running {
		t.Fatalf("initial status = %+v, %v", st, err)
	}
	spec := daemonproto.BotSpec{Kind: "telegram", CWD: "/w", Env: []string{"A=1"}, Args: []string{"--model", "m"}}
	if err := bt.c.call(t, daemonproto.BotStartRequest{Type: "bot_start", Spec: spec}, &st); err != nil {
		t.Fatal(err)
	}
	if !st.Running || st.Session != "s1" || st.PairedUserID != 7 {
		t.Fatalf("start status = %+v", st)
	}
	if err := bt.c.call(t, daemonproto.BotStartRequest{Type: "bot_start", Spec: spec}, nil); err == nil || !strings.Contains(err.Error(), ErrBotRunning.Error()) {
		t.Fatalf("second start = %v", err)
	}
	if got := readState(t, bt.home); len(got.Sessions) != 1 || !got.Sessions[0].Headless || got.Sessions[0].CWD != "/w" {
		t.Fatalf("bot session state = %+v", got)
	}

	waitSubs(t, s1, 2)
	bt.tg.dm("hello bot")
	in := recv(t, s1.inputs)
	if in.Text != "hello bot" || in.Origin != session.InputOriginTelegram {
		t.Fatalf("bot input = %+v", in)
	}
	turn(s1, 10, session.InputOriginTelegram, "", "hi from zut")
	for {
		if got := recv(t, bt.tg.sent); got == "hi from zut" {
			break
		}
	}

	st = daemonproto.BotStatusData{}
	if err := bt.c.call(t, daemonproto.BotStopRequest{Type: "bot_stop", Kind: "telegram"}, &st); err != nil || st.Running {
		t.Fatalf("stop = %+v, %v", st, err)
	}
	if reason := recv(t, s1.closes); reason != botStopReason {
		t.Fatalf("close reason = %q", reason)
	}
	if rec := loadBotRecord(bt.home); len(rec) != 0 {
		t.Fatalf("bot record after stop = %v", rec)
	}
	st = daemonproto.BotStatusData{}
	if err := bt.c.call(t, daemonproto.BotStatusRequest{Type: "bot_status", Kind: "telegram"}, &st); err != nil || st.Running {
		t.Fatalf("status after stop = %+v, %v", st, err)
	}
	bt.r.stop(t)
	if strings.Contains(bt.logs.String(), "SECRET") {
		t.Fatalf("bot token leaked into logs:\n%s", bt.logs.String())
	}
	if fi, err := os.Stat(telegram.ConfigPath(bt.home)); err != nil || fi.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Fatalf("bot.json mode = %v, %v", fi.Mode(), err)
	}
}

func TestBotStartRequiresConfiguration(t *testing.T) {
	home := t.TempDir()
	f := newMockBotFactory(t)
	svc := &telegram.Service{Home: home}
	f.EXPECT().CheckBot("telegram").RunAndReturn(func(string) error { return svc.Check() }).Once()
	r := startDaemon(t, f, home, Options{})
	c := dial(t, r.sock)
	err := c.call(t, daemonproto.BotStartRequest{Type: "bot_start", Spec: daemonproto.BotSpec{Kind: "telegram", CWD: "/w"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("start unconfigured = %v", err)
	}
}

func TestTelegramBridgeOnOff(t *testing.T) {
	s1 := newTestSession(t, "s1")
	bt := newBotTest(t, s1)
	var info session.Info
	if err := bt.c.call(t, daemonproto.CreateRequest{Type: "create", Spec: session.CreateSpec{CWD: "/w"}}, &info); err != nil {
		t.Fatal(err)
	}
	var br daemonproto.TelegramBridgeData
	if err := bt.c.call(t, daemonproto.TelegramBridgeRequest{Type: "telegram_bridge", Session: "s1", On: true}, &br); err != nil {
		t.Fatal(err)
	}
	if !br.Active || br.Session != "s1" || br.Username != "zutbot" {
		t.Fatalf("bridge on = %+v", br)
	}
	// The bridge and a bot service would race for the same updates.
	if err := bt.c.call(t, daemonproto.BotStartRequest{Type: "bot_start", Spec: daemonproto.BotSpec{Kind: "telegram", CWD: "/w"}}, nil); err == nil {
		t.Fatal("bot_start succeeded while a bridge polls the bot")
	}

	// No client is attached: the bridge works detached.
	waitSubs(t, s1, 2)
	bt.tg.dm("from phone")
	if in := recv(t, s1.inputs); in.Text != "from phone" || in.Origin != session.InputOriginTelegram {
		t.Fatalf("bridged input = %+v", in)
	}
	turn(s1, 10, session.InputOriginTelegram, "from phone", "phone answer")
	turn(s1, 20, session.InputOriginUser, "typed in tui", "tui answer")
	want := []string{"phone answer", "you: typed in tui", "tui answer"}
	for _, w := range want {
		if got := recv(t, bt.tg.sent); got != w {
			t.Fatalf("mirrored %q, want %q", got, w)
		}
	}

	var st daemonproto.BotStatusData
	if err := bt.c.call(t, daemonproto.BotStatusRequest{Type: "bot_status", Kind: "telegram"}, &st); err != nil || st.Bridge == nil || st.Bridge.Session != "s1" {
		t.Fatalf("status with bridge = %+v, %v", st, err)
	}
	br = daemonproto.TelegramBridgeData{}
	if err := bt.c.call(t, daemonproto.TelegramBridgeRequest{Type: "telegram_bridge", Session: "s1", On: false}, &br); err != nil || br.Active {
		t.Fatalf("bridge off = %+v, %v", br, err)

	}
	st = daemonproto.BotStatusData{}
	if err := bt.c.call(t, daemonproto.BotStatusRequest{Type: "bot_status", Kind: "telegram"}, &st); err != nil || st.Bridge != nil {
		t.Fatalf("status after off = %+v, %v", st, err)
	}
	if err := bt.c.call(t, daemonproto.TelegramBridgeRequest{Type: "telegram_bridge", Session: "missing", On: true}, nil); err == nil {
		t.Fatal("bridge on unknown session succeeded")
	}
	bt.r.stop(t)
	if strings.Contains(bt.logs.String(), "SECRET") {
		t.Fatal("bot token leaked into logs")
	}
}

func TestBotRequestsWithoutBotHost(t *testing.T) {
	r := startDaemon(t, nil, t.TempDir(), Options{})
	c := dial(t, r.sock)
	if err := c.call(t, daemonproto.BotStatusRequest{Type: "bot_status"}, nil); err == nil || !strings.Contains(err.Error(), errNoBotHost.Error()) {
		t.Fatalf("bot_status = %v", err)
	}
}

func TestBotStopRacingStart(t *testing.T) {
	s1 := newTestSession(t, "s1")
	bt := newBotTest(t, s1)
	spec := daemonproto.BotSpec{Kind: "telegram", CWD: "/w"}
	stopper := dial(t, bt.r.sock)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = bt.c.call(t, daemonproto.BotStartRequest{Type: "bot_start", Spec: spec}, nil)
	}()
	go func() {
		defer wg.Done()
		// May observe nothing, a starting bot (retry error) or a running one.
		_ = stopper.call(t, daemonproto.BotStopRequest{Type: "bot_stop", Kind: "telegram"}, nil)
	}()
	wg.Wait()
	var st daemonproto.BotStatusData
	if err := bt.c.call(t, daemonproto.BotStopRequest{Type: "bot_stop", Kind: "telegram"}, &st); err != nil || st.Running {
		t.Fatalf("final stop = %+v, %v", st, err)
	}
}

func TestBotStartRacingShutdown(t *testing.T) {
	home := t.TempDir()
	if err := telegram.SaveConfig(home, telegram.Config{BotToken: testBotToken}); err != nil {
		t.Fatal(err)
	}
	svc := &telegram.Service{Home: home}
	s1 := newTestSession(t, "s1")
	f := newMockBotFactory(t)
	inCreate, release := make(chan struct{}), make(chan struct{})
	f.EXPECT().CheckBot("telegram").RunAndReturn(func(string) error { return svc.Check() }).Once()
	f.EXPECT().Create(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, session.CreateSpec) (session.Handle, error) {
		close(inCreate)
		<-release
		return s1.h, nil
	}).Once()
	r := startDaemon(t, f, home, Options{})
	c := dial(t, r.sock)
	errc := make(chan error, 1)
	go func() {
		errc <- c.call(t, daemonproto.BotStartRequest{Type: "bot_start", Spec: daemonproto.BotSpec{Kind: "telegram", CWD: "/w"}}, nil)
	}()
	recv(t, inCreate)
	r.d.Stop()
	for !r.d.isStopping() {
		time.Sleep(time.Millisecond)
	}
	close(release)
	// The bot never runs (RunBot has no expectation) and its session is
	// closed instead of being left for restore.
	recv(t, s1.closes)
	r.wait(t)
	<-errc
	deadline := time.Now().Add(5 * time.Second)
	for len(readState(t, home).Sessions) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("bot session kept in state.json: %+v", readState(t, home))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rec := loadBotRecord(home); len(rec) != 0 {
		t.Fatalf("bot record kept: %v", rec)
	}
}
