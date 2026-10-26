package modes

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/dictation"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/mattn/go-runewidth"
)

type dictationTranscribeFunc func(context.Context, provider.AudioInput) (string, error)

func (f dictationTranscribeFunc) Transcribe(ctx context.Context, a provider.AudioInput) (string, error) {
	return f(ctx, a)
}

type fakeDictationRecording struct {
	result         dictation.Result
	done           chan struct{}
	canceled       chan struct{}
	once           sync.Once
	err            error
	cleanupEntered chan struct{}
	cleanupRelease <-chan struct{}
}

func (r *fakeDictationRecording) Level() float64                  { return 0.7 }
func (r *fakeDictationRecording) Recorder() string                { return "pw-record" }
func (r *fakeDictationRecording) Stop() (dictation.Result, error) { return r.result, r.err }
func (r *fakeDictationRecording) Cancel() {
	r.once.Do(func() {
		if r.cleanupEntered != nil {
			close(r.cleanupEntered)
			<-r.cleanupRelease
		}
		_ = r.result.Cleanup()
		close(r.canceled)
	})
}
func (r *fakeDictationRecording) Done() <-chan struct{} { return r.done }

var dictationToggle = tui.Key{Kind: tui.KeyRune, Rune: 'v', Alt: true}

func dictationHarness(t *testing.T, transcribe dictationTranscribeFunc) (*Interactive, *fakeDictationRecording, <-chan struct{}) {
	t.Helper()
	path := t.TempDir() + "/audio.wav"
	if err := os.WriteFile(path, []byte("fake audio"), 0600); err != nil {
		t.Fatal(err)
	}
	r := &fakeDictationRecording{result: dictation.Result{Path: path, Filename: "audio.wav", MIMEType: "audio/wav"}, done: make(chan struct{}), canceled: make(chan struct{})}
	started := make(chan struct{})
	i := NewInteractive(InteractiveConfig{Terminal: &cleanupTestTerminal{}, Theme: tui.Dark, Dictation: DictationConfig{Service: "ChatGPT", Transcriber: transcribe, Language: "fr", Start: func(ctx context.Context, o dictation.Options) (DictationRecording, error) {
		if o.MaxDuration != 10*time.Minute {
			t.Error("missing default duration")
		}
		close(started)
		return r, nil
	}}})
	t.Cleanup(i.shutdownDictation)
	return i, r, started
}
func waitDictation(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("dictation worker did not finish")
	}
}
func finishDictation(t *testing.T, i *Interactive) {
	t.Helper()
	before := i.ed.Value()
	waitDictation(t, i.dictation.done)
	if i.ed.Value() != before {
		t.Fatal("worker edited the input outside the UI loop")
	}
	i.refreshDictation()
}
func TestDictationToggleInsert(t *testing.T) {
	for _, tc := range []struct {
		before string
		cursor int
		want   string
	}{{"hello!", 5, "hello words!"}, {"hello !", 6, "hello words!"}, {"", 0, "words"}, {"é!", 1, "é words!"}, {"first\n!", 0, "first\nwords!"}} {
		t.Run(tc.want, func(t *testing.T) {
			i, r, started := dictationHarness(t, func(ctx context.Context, a provider.AudioInput) (string, error) {
				if a.Language != "fr" {
					t.Error("language not forwarded")
				}
				return "  words \n", nil
			})
			i.ed.SetValue(tc.before)
			i.ed.CursorC = tc.cursor
			i.busy = true // Dictation is independent of an active streaming turn.
			i.handleKey(context.Background(), dictationToggle)
			waitDictation(t, started)
			i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: 'V', Alt: true, Shift: true})
			finishDictation(t, i)
			if got := i.ed.Value(); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
			if !i.busy {
				t.Fatal("changed active turn")
			}
			if _, err := os.Stat(r.result.Path); !os.IsNotExist(err) {
				t.Fatal("audio not deleted")
			}
		})
	}
}
func TestDictationEscapeCancels(t *testing.T) {
	i, r, started := dictationHarness(t, func(context.Context, provider.AudioInput) (string, error) {
		t.Error("uploaded canceled recording")
		return "", nil
	})
	i.ed.SetValue("keep")
	i.handleKey(context.Background(), dictationToggle)
	waitDictation(t, started)
	i.doubleEscape.Arm(i.clock())
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc})
	waitDictation(t, r.canceled)
	if i.doubleEscape.armed || i.ed.Value() != "keep" || i.dictation.phase != "" {
		t.Fatal("Esc fell through or did not cancel")
	}
	if _, err := os.Stat(r.result.Path); !os.IsNotExist(err) {
		t.Fatal("audio not removed")
	}
}
func TestDictationRunExitCancelsRecording(t *testing.T) {
	i, r, started := dictationHarness(t, func(context.Context, provider.AudioInput) (string, error) {
		t.Error("uploaded on exit")
		return "", nil
	})
	i.handleKey(context.Background(), dictationToggle)
	waitDictation(t, started)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := i.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	waitDictation(t, r.canceled)
	if _, err := os.Stat(r.result.Path); !os.IsNotExist(err) {
		t.Fatal("audio not removed on Run exit")
	}
}

func TestDictationAutoStop(t *testing.T) {
	i, r, started := dictationHarness(t, func(context.Context, provider.AudioInput) (string, error) { return "auto", nil })
	i.handleKey(context.Background(), dictationToggle)
	waitDictation(t, started)
	close(r.done)
	finishDictation(t, i)
	if i.ed.Value() != "auto" {
		t.Fatal(i.ed.Value())
	}
}
func TestDictationTranscribingIgnoresToggleAndExitCancels(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	i, r, started := dictationHarness(t, func(ctx context.Context, a provider.AudioInput) (string, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		return "", ctx.Err()
	})
	i.handleKey(context.Background(), dictationToggle)
	waitDictation(t, started)
	i.handleKey(context.Background(), dictationToggle)
	waitDictation(t, entered)
	i.handleKey(context.Background(), dictationToggle)
	if i.dictation.phase != "transcribing" {
		t.Fatal("restarted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := i.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	waitDictation(t, canceled)
	waitDictation(t, r.canceled)
	if _, err := os.Stat(r.result.Path); !os.IsNotExist(err) {
		t.Fatal("audio not removed")
	}
	if i.ed.Value() != "" {
		t.Fatal("inserted after exit")
	}
}
func TestDictationErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{{provider.ErrTranscribeUnauthorized, "ChatGPT login expired, run /login"}, {provider.ErrTranscribeEmpty, "No speech detected"}, {ErrDictationLogin, "Dictation needs a ChatGPT login (/login)"}, {errors.New("response body secret"), "Dictation failed, try again"}} {
		t.Run(tc.want, func(t *testing.T) {
			i, r, started := dictationHarness(t, func(context.Context, provider.AudioInput) (string, error) { return "", tc.err })
			i.handleKey(context.Background(), dictationToggle)
			waitDictation(t, started)
			i.handleKey(context.Background(), dictationToggle)
			finishDictation(t, i)
			if i.statusErr != tc.want {
				t.Fatal(i.statusErr)
			}
			if _, err := os.Stat(r.result.Path); !os.IsNotExist(err) {
				t.Fatal("audio not removed")
			}
		})
	}
	t.Run("start", func(t *testing.T) {
		i := NewInteractive(InteractiveConfig{Dictation: DictationConfig{Transcriber: dictationTranscribeFunc(nil), Start: func(context.Context, dictation.Options) (DictationRecording, error) {
			return nil, dictation.ErrNoRecorder
		}}})
		i.handleKey(context.Background(), dictationToggle)
		finishDictation(t, i)
		if i.statusErr != "No recorder found: install pw-record, parecord or ffmpeg" {
			t.Fatal(i.statusErr)
		}
	})
	t.Run("stop", func(t *testing.T) {
		i, r, started := dictationHarness(t, func(context.Context, provider.AudioInput) (string, error) {
			t.Error("transcribed failed recorder")
			return "", nil
		})
		r.err = &dictation.RecorderError{Recorder: "pw-record"}
		i.handleKey(context.Background(), dictationToggle)
		waitDictation(t, started)
		close(r.done)
		finishDictation(t, i)
		if i.statusErr != "Recording failed (pw-record)" {
			t.Fatal(i.statusErr)
		}
	})
}
func TestDictationOverlayAndIndicator(t *testing.T) {
	i, _, _ := dictationHarness(t, func(context.Context, provider.AudioInput) (string, error) { return "", nil })
	i.helpBlock = []string{"help"}
	i.handleKey(context.Background(), dictationToggle)
	if i.dictation.phase != "" || i.ed.Value() != "" {
		t.Fatal("started in overlay")
	}
	now := time.Unix(100, 0)
	i.clock = func() time.Time { return now }
	i.dictation = interactiveDictation{phase: "recording", started: now.Add(-7 * time.Second)}
	lines := i.dictationIndicatorLocked(80)
	frame := strings.Join(lines, "")
	now = now.Add(480 * time.Millisecond)
	if strings.Join(i.dictationIndicatorLocked(80), "") != frame {
		t.Fatal("recording dot changed")
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "REC 00:07") {
		t.Fatal(lines)
	}
	for _, width := range []int{1, 8, 20} {
		for _, line := range i.dictationIndicatorLocked(width) {
			if runewidth.StringWidth(stripANSIBytes(line)) > width {
				t.Fatalf("indicator exceeds width %d: %q", width, line)
			}
		}
	}
	i.dictation.phase = "transcribing"
	if !strings.Contains(i.dictationIndicatorLocked(80)[0], "Transcribing 0 s") {
		t.Fatal("missing spinner")
	}
	i.dictation = interactiveDictation{}
}

func TestDictationSanitizeTranscript(t *testing.T) {
	for _, text := range []string{"\x1b]52;c;ZXZpbA==\x07hello", "\x1b[31m\x07"} {
		i, _, started := dictationHarness(t, func(context.Context, provider.AudioInput) (string, error) { return text, nil })
		i.handleKey(context.Background(), dictationToggle)
		waitDictation(t, started)
		i.handleKey(context.Background(), dictationToggle)
		finishDictation(t, i)
		if strings.HasSuffix(text, "hello") {
			if i.ed.Value() != "hello" {
				t.Fatal(i.ed.Value())
			}
		} else if i.statusErr != "No speech detected" {
			t.Fatal(i.statusErr)
		}
	}
}

func TestDictationEscapeDoesNotWaitForCleanup(t *testing.T) {
	i, r, started := dictationHarness(t, func(context.Context, provider.AudioInput) (string, error) { return "", nil })
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	r.cleanupEntered = make(chan struct{})
	r.cleanupRelease = release
	i.handleKey(context.Background(), dictationToggle)
	waitDictation(t, started)
	escaped := make(chan struct{})
	go func() { i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); close(escaped) }()
	waitDictation(t, escaped)
	waitDictation(t, r.cleanupEntered)
	// A second recording can start while the first worker is still in Cancel.
	secondStarted := make(chan struct{})
	second := &fakeDictationRecording{done: make(chan struct{}), canceled: make(chan struct{})}
	i.cfg.Dictation.Start = func(context.Context, dictation.Options) (DictationRecording, error) {
		close(secondStarted)
		return second, nil
	}
	i.handleKey(context.Background(), dictationToggle)
	waitDictation(t, secondStarted)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	exited := make(chan struct{})
	go func() { _ = i.Run(ctx); close(exited) }()
	waitDictation(t, second.canceled)
	select {
	case <-exited:
		t.Fatal("Run returned before pending cleanup")
	default:
	}
	unblock()
	waitDictation(t, exited)
}

func TestDictationRefreshDoesNotWaitForCleanup(t *testing.T) {
	i, r, started := dictationHarness(t, func(context.Context, provider.AudioInput) (string, error) { return "hello", nil })
	release := make(chan struct{})
	defer close(release)
	r.cleanupEntered = make(chan struct{})
	r.cleanupRelease = release
	i.handleKey(context.Background(), dictationToggle)
	waitDictation(t, started)
	i.handleKey(context.Background(), dictationToggle)
	waitDictation(t, r.cleanupEntered)
	refreshed := make(chan struct{})
	go func() { i.refreshDictation(); close(refreshed) }()
	waitDictation(t, refreshed)
	if i.ed.Value() != "hello" || i.dictation.phase != "" {
		t.Fatal("terminal event not applied")
	}
	// The render mutex is available even though the worker is still cleaning up.
	i.mu.Lock()
	pending := len(i.pendingDictation)
	i.mu.Unlock()
	if pending != 1 {
		t.Fatalf("pending workers = %d, want 1", pending)
	}
}

func TestDictationStructuredErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{dictation.ErrNoRecorder, "No recorder found: install pw-record, parecord or ffmpeg"},
		{dictation.ErrOutputPlaceholder, "dictation.recorder needs an {output} placeholder"},
		{dictation.ErrTooLittleAudio, "Recording too short or microphone silent"},
		{dictation.ErrFinalize, "Recording could not be finalized"},
		{&dictation.RecorderError{Recorder: "pw-record"}, "Recording failed (pw-record)"},
		{&dictation.RecorderError{Recorder: "pw-record", Hint: "device unavailable"}, "Recording failed (pw-record): device unavailable"},
		{&provider.TranscribeHTTPError{StatusCode: 500}, "ChatGPT transcription failed (HTTP 500)"},
		{provider.ErrTranscribeNetwork, "ChatGPT transcription failed: network error"},
		{provider.ErrTranscribeResponse, "ChatGPT transcription failed: unexpected response"},
		{context.DeadlineExceeded, "Transcription timed out"},
	} {
		if got := dictationError(tc.err, "ChatGPT"); got != tc.want {
			t.Errorf("%v: %q", tc.err, got)
		}
	}
}

func TestDictationIndicatorMetadata(t *testing.T) {
	i, r, _ := dictationHarness(t, func(context.Context, provider.AudioInput) (string, error) { return "", nil })
	now := time.Unix(100, 0)
	i.clock = func() time.Time { return now }
	i.dictation = interactiveDictation{phase: "recording", started: now.Add(-7 * time.Second), recording: r, history: []rune("▁▂▅▇▅▃▁▁")}
	full := stripANSIBytes(i.dictationIndicatorLocked(100)[0])
	if full != "● REC 00:07 · pw-record ▁▂▅▇▅▃▁▁  Alt+V stop · Esc cancel" {
		t.Fatal(full)
	}
	narrow := stripANSIBytes(i.dictationIndicatorLocked(35)[0])
	if strings.Contains(narrow, "Alt+V") || !strings.Contains(narrow, "▁▂") {
		t.Fatal(narrow)
	}
	narrow = stripANSIBytes(i.dictationIndicatorLocked(23)[0])
	if strings.Contains(narrow, "▁") {
		t.Fatal(narrow)
	}
	i.dictation.phase = "transcribing"
	i.dictation.audio.Duration = 7 * time.Second
	i.dictation.size = 224 * 1024
	if got := stripANSIBytes(i.dictationIndicatorLocked(100)[0]); !strings.Contains(got, " Transcribing 7 s · 224 KB → ChatGPT") {
		t.Fatal(got)
	}
	i.dictation = interactiveDictation{}
}

func TestDictationSuccessStatus(t *testing.T) {
	i, _, started := dictationHarness(t, func(context.Context, provider.AudioInput) (string, error) { return "hello world", nil })
	i.handleKey(context.Background(), dictationToggle)
	waitDictation(t, started)
	i.handleKey(context.Background(), dictationToggle)
	finishDictation(t, i)
	if i.statusOK != "2 words · ChatGPT · 0.0 s" || i.statusErr != "" {
		t.Fatal(i.statusOK, i.statusErr)
	}
}
