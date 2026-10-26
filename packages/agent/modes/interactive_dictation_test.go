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
	result   dictation.Result
	done     chan struct{}
	canceled chan struct{}
	once     sync.Once
	err      error
}

func (r *fakeDictationRecording) Stop() (dictation.Result, error) { return r.result, r.err }
func (r *fakeDictationRecording) Cancel() {
	r.once.Do(func() { _ = r.result.Cleanup(); close(r.canceled) })
}
func (r *fakeDictationRecording) Done() <-chan struct{}  { return r.done }
func (r *fakeDictationRecording) Elapsed() time.Duration { return 7 * time.Second }

var dictationToggle = tui.Key{Kind: tui.KeyRune, Rune: 'v', Alt: true}

func dictationHarness(t *testing.T, transcribe dictationTranscribeFunc) (*Interactive, *fakeDictationRecording, <-chan struct{}) {
	t.Helper()
	path := t.TempDir() + "/audio.wav"
	if err := os.WriteFile(path, []byte("fake audio"), 0600); err != nil {
		t.Fatal(err)
	}
	r := &fakeDictationRecording{result: dictation.Result{Path: path, Filename: "audio.wav", MIMEType: "audio/wav"}, done: make(chan struct{}), canceled: make(chan struct{})}
	started := make(chan struct{})
	i := NewInteractive(InteractiveConfig{Terminal: &cleanupTestTerminal{}, Theme: tui.Dark, Dictation: DictationConfig{Transcriber: transcribe, Language: "fr", Start: func(ctx context.Context, o dictation.Options) (DictationRecording, error) {
		if o.MaxDuration != 10*time.Minute {
			t.Error("missing default duration")
		}
		close(started)
		return r, nil
	}}})
	t.Cleanup(i.cancelDictation)
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
			return nil, errors.New("dictation: no recorder found")
		}}})
		i.handleKey(context.Background(), dictationToggle)
		finishDictation(t, i)
		if i.statusErr != "dictation: no recorder found" {
			t.Fatal(i.statusErr)
		}
	})
	t.Run("stop", func(t *testing.T) {
		i, r, started := dictationHarness(t, func(context.Context, provider.AudioInput) (string, error) {
			t.Error("transcribed failed recorder")
			return "", nil
		})
		r.err = errors.New("dictation: recorder failed")
		i.handleKey(context.Background(), dictationToggle)
		waitDictation(t, started)
		close(r.done)
		finishDictation(t, i)
		if i.statusErr != "dictation: recorder failed" {
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
	if strings.Join(i.dictationIndicatorLocked(80), "") == frame {
		t.Fatal("recording dot did not blink")
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
	if !strings.Contains(i.dictationIndicatorLocked(80)[0], "Transcribing…") {
		t.Fatal("missing spinner")
	}
	i.dictation = interactiveDictation{}
}
