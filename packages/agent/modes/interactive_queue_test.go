package modes

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestQueuedMessageSummaryKeepsImageIndicatorWhenTextIsTruncated(t *testing.T) {
	message := core.QueuedMessage{
		Text:   strings.Repeat("x", 100),
		Images: []provider.ImageBlock{{MimeType: "image/png", Data: []byte("png-1")}},
	}

	summary := queuedMessageSummary(message, 24)

	if !strings.HasSuffix(summary, " [image]") {
		t.Fatalf("summary = %q, want visible image indicator", summary)
	}
	if len([]rune(summary)) > 24 {
		t.Fatalf("summary width = %d, want at most 24", len([]rune(summary)))
	}
}

func TestBusySubmitQueuesClipboardImagePrompt(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	submitted := make(chan session.Input, 1)
	port.EXPECT().Submit(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in session.Input) error {
		submitted <- in
		return nil
	}).Once()
	i := newQueuePortInteractive(t, port, session.Snapshot{Info: session.Info{Run: session.RunStateRunning}}, InteractiveConfig{})
	i.ed.SetValue("inspect [clipboard image #1]")
	i.clipboardImages = []clipboardImageAttachment{testClipboardImage("[clipboard image #1]", "png-1")}

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})

	// The daemon decides whether a running session queues the prompt.
	in := recvInput(t, submitted)
	if in.Text != "inspect" || in.Mode != session.SubmitModeAuto {
		t.Fatalf("submitted = %#v, want one auto-mode inspect prompt", in)
	}
	if len(in.Images) != 1 || string(in.Images[0].Data) != "png-1" {
		t.Fatalf("submitted images = %#v, want png-1", in.Images)
	}
	if !i.ed.IsEmpty() || len(i.clipboardImages) != 0 {
		t.Fatal("submitted image prompt remained in the editor")
	}
}

func TestSlideBackRestoresQueuedImagesToEditor(t *testing.T) {
	image := testClipboardImage("unused", "png-1").Image
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().SlideBackQueued(mock.Anything).Return(session.Input{Text: "inspect", Images: []provider.ImageBlock{image}}, nil).Once()
	i := newQueuePortInteractive(t, port, session.Snapshot{
		Info:  session.Info{Run: session.RunStateRunning},
		Queue: []session.QueuedInput{{Input: session.Input{Text: "inspect", Images: []provider.ImageBlock{image}}}},
	}, InteractiveConfig{})

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyUp, Alt: true})
	runLoopCall(t, i)

	if got := i.ed.Value(); got != "inspect [clipboard image #1]" {
		t.Fatalf("editor = %q, want restored image marker", got)
	}
	if len(i.clipboardImages) != 1 || string(i.clipboardImages[0].Image.Data) != "png-1" {
		t.Fatalf("clipboard images = %#v, want png-1", i.clipboardImages)
	}
}

// Which queued item slides back (never host evidence) is daemon policy:
// session TestSlideBackQueuedPreservesHostEventsAndFallsBackToAgent. The client
// must not even ask the daemon when only host reports are queued.
func TestSlideBackSkipsHostEvents(t *testing.T) {
	port := sessionmocks.NewMockPort(t) // no SlideBackQueued expectation
	i := newQueuePortInteractive(t, port, session.Snapshot{
		Info: session.Info{Run: session.RunStateRunning},
		Queue: []session.QueuedInput{
			{Input: session.Input{Text: "first report", HostEvent: true}},
			{Input: session.Input{Text: "latest report", HostEvent: true}},
		},
	}, InteractiveConfig{})
	i.ed.SetValue("draft")

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyUp, Alt: true})

	if i.portRecall != nil {
		t.Fatal("host-only queue started a slide-back")
	}
	if got := i.ed.Value(); got != "draft" {
		t.Fatalf("host-only queue changed editor to %q", got)
	}
}

func TestSlideBackParsesModifiedArrow(t *testing.T) {
	for name, sequence := range map[string]string{
		"alt up":       "\x1b[1;3A",
		"alt shift up": "\x1b[1;4A",
	} {
		t.Run(name, func(t *testing.T) {
			port := sessionmocks.NewMockPort(t)
			port.EXPECT().SlideBackQueued(mock.Anything).Return(session.Input{Text: "recover this draft"}, nil).Once()
			i := newQueuePortInteractive(t, port, session.Snapshot{
				Info: session.Info{Run: session.RunStateRunning},
				Queue: []session.QueuedInput{
					{Input: session.Input{Text: "recover this draft"}},
					{Input: session.Input{Text: "worker report", HostEvent: true}},
				},
			}, InteractiveConfig{})
			reader := tui.NewReader(func() (byte, error) {
				if sequence == "" {
					return 0, io.EOF
				}
				b := sequence[0]
				sequence = sequence[1:]
				return b, nil
			})
			key, err := reader.Read()
			if err != nil {
				t.Fatal(err)
			}
			i.handleKey(context.Background(), key)
			runLoopCall(t, i)
			if got := i.ed.Value(); got != "recover this draft" {
				t.Fatalf("editor = %q, want user draft", got)
			}
		})
	}
}

// With only host reports queued the daemon refuses the slide-back
// (ErrUnknownRequest, session TestSlideBackQueuedPreservesHostEventsAndFallsBackToAgent);
// Escape must still abort and keep the existing draft.
func TestEscapeSkipsHostEvents(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().SlideBackQueued(mock.Anything).Return(session.Input{}, session.ErrUnknownRequest).Once()
	aborted := make(chan struct{})
	port.EXPECT().Abort(mock.Anything).RunAndReturn(func(context.Context) error { close(aborted); return nil }).Once()
	i := newQueuePortInteractive(t, port, session.Snapshot{
		Info:  session.Info{Run: session.RunStateRunning},
		Queue: []session.QueuedInput{{Input: session.Input{Text: "worker report", HostEvent: true}}},
	}, InteractiveConfig{})
	i.ed.SetValue("existing draft")

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc})
	runLoopCall(t, i)
	waitSignal(t, aborted, "Abort")

	if got := i.ed.Value(); got != "existing draft" {
		t.Fatalf("editor = %q, want existing draft", got)
	}
}

func TestSlidingQueueHintOnlyForUserMessages(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "vev")
	for _, hasUserMessage := range []bool{false, true} {
		queue := []session.QueuedInput{{Input: session.Input{Text: "worker report", HostEvent: true}}}
		if hasUserMessage {
			queue = append(queue, session.QueuedInput{Input: session.Input{Text: "user draft"}})
		}
		term := &alertTestTerminal{}
		i := newQueuePortInteractive(t, sessionmocks.NewMockPort(t), session.Snapshot{
			Info:  session.Info{Run: session.RunStateRunning},
			Queue: queue,
		}, InteractiveConfig{Terminal: term, Theme: tui.Dark})
		i.rend.Resize(80, 24)
		i.redraw()
		output := stripANSIBytes(term.String())
		if !strings.Contains(output, "worker report") {
			t.Fatal("host report missing from sliding queue")
		}
		if got := strings.Contains(output, "Press Alt+Shift+↑"); got != hasUserMessage {
			t.Fatalf("user=%v: recall hint visible=%v", hasUserMessage, got)
		}
	}
}

// newQueuePortInteractive attaches cfg to port, hydrated from snap. It does
// not start Run: tests drive keys directly and apply loop callbacks with
// runLoopCall.
func newQueuePortInteractive(t *testing.T, port *sessionmocks.MockPort, snap session.Snapshot, cfg InteractiveConfig) *Interactive {
	t.Helper()
	snap.Seq = 1
	port.EXPECT().Snapshot(mock.Anything).Return(snap, nil).Once()
	port.EXPECT().Subscribe(mock.Anything, uint64(1)).Return(make(chan session.Event), nil).Once()
	cfg.Port = port
	i := NewInteractive(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, i.attachPort(ctx))
	return i
}

// runLoopCall applies the next callback a session worker posted to the loop.
func runLoopCall(t *testing.T, i *Interactive) {
	t.Helper()
	select {
	case fn := <-i.loopCalls:
		fn()
	case <-time.After(5 * time.Second):
		t.Fatal("missing loop callback")
	}
}

func recvInput(t *testing.T, ch <-chan session.Input) session.Input {
	t.Helper()
	select {
	case in := <-ch:
		return in
	case <-time.After(5 * time.Second):
		t.Fatal("Submit was not called")
		return session.Input{}
	}
}

// /session fork follows /fork onto the daemon path; the legacy gate needs a
// local agent and would always refuse with "wait until ... queued messages".
func TestPortSessionForkOpensForkPicker(t *testing.T) {
	i := newQueuePortInteractive(t, sessionmocks.NewMockPort(t), session.Snapshot{
		Info:     session.Info{Run: session.RunStateIdle, SessionPath: "/session.jsonl"},
		Messages: []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "fork from here"}}}},
	}, InteractiveConfig{})

	i.runSlash(context.Background(), "/session fork")

	if !i.pendingFork || i.statusErr != "" {
		t.Fatalf("fork picker not opened: pendingFork=%v statusErr=%q", i.pendingFork, i.statusErr)
	}
}
