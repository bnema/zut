package modes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/bnema/zut/packages/agent/dictation"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// ErrDictationLogin means no ChatGPT OAuth credential is available.
var ErrDictationLogin = errors.New("dictation needs login")

// DictationRecording is the lifecycle owned by one interactive dictation.
type DictationRecording interface {
	Stop() (dictation.Result, error)
	Cancel()
	Done() <-chan struct{}
	Elapsed() time.Duration
}

type DictationConfig struct {
	Transcriber provider.Transcriber
	Recorder    dictation.Options
	Language    string
	// Start overrides recorder creation for embedded hosts and tests.
	Start func(context.Context, dictation.Options) (DictationRecording, error)
}

type dictationEvent struct {
	transcribing bool
	text         string
	err          error
}

// All state belongs to the UI loop and is protected by i.mu for rendering.
// Workers publish immutable events; they never access the editor or UI state.
type interactiveDictation struct {
	phase   string
	started time.Time
	stop    chan struct{}
	events  <-chan dictationEvent
	cancel  context.CancelFunc
	done    <-chan struct{}
}

func (i *Interactive) handleDictationKey(ctx context.Context, k tui.Key) bool {
	i.mu.Lock()
	recording := i.dictation.phase == "recording"
	if k.Kind == tui.KeyEsc && recording {
		i.doubleEscape.Reset()
		i.mu.Unlock()
		i.cancelDictation()
		return true
	}
	if k.Kind != tui.KeyRune || !k.Alt || (k.Rune != 'v' && k.Rune != 'V') {
		i.mu.Unlock()
		return false
	}
	// Never let Alt+V become text in an overlay or the editor.
	if i.confirmChildActiveLocked() || i.confirmDialog.Active() {
		i.mu.Unlock()
		return true
	}
	if i.dictation.phase != "" {
		if recording {
			close(i.dictation.stop)
			i.dictation.stop = nil
			i.dictation.phase = "transcribing"
		}
		i.mu.Unlock()
		i.invalidate()
		return true
	}
	cfg := i.cfg.Dictation
	if cfg.Transcriber == nil {
		i.statusErr = "Dictation needs a ChatGPT login (/login)"
		i.mu.Unlock()
		i.invalidate()
		return true
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	events := make(chan dictationEvent, 3)
	stop := make(chan struct{})
	done := make(chan struct{})
	i.dictation = interactiveDictation{phase: "recording", started: i.clock(), stop: stop, events: events, cancel: cancel, done: done}
	i.mu.Unlock()
	go runDictation(sessionCtx, cfg, stop, events, done)
	i.invalidate()
	return true
}

func runDictation(ctx context.Context, cfg DictationConfig, stop <-chan struct{}, events chan<- dictationEvent, done chan<- struct{}) {
	defer close(done)
	start := cfg.Start
	if start == nil {
		start = func(ctx context.Context, opts dictation.Options) (DictationRecording, error) {
			return dictation.Start(ctx, opts)
		}
	}
	if cfg.Recorder.MaxDuration == 0 {
		cfg.Recorder.MaxDuration = 10 * time.Minute
	}
	recording, err := start(ctx, cfg.Recorder)
	if err != nil {
		events <- dictationEvent{err: err}
		return
	}
	defer recording.Cancel()
	select {
	case <-ctx.Done():
		return
	case <-stop:
	case <-recording.Done():
	}
	if ctx.Err() != nil {
		return
	}
	events <- dictationEvent{transcribing: true}
	audio, err := recording.Stop()
	if err != nil {
		events <- dictationEvent{err: err}
		return
	}
	defer audio.Cleanup()
	if ctx.Err() != nil {
		return
	}
	f, err := os.Open(audio.Path)
	if err != nil {
		events <- dictationEvent{err: errors.New("Could not read dictation audio")}
		return
	}
	defer f.Close()
	requestCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	text, err := cfg.Transcriber.Transcribe(requestCtx, provider.AudioInput{Filename: audio.Filename, MIMEType: audio.MIMEType, Reader: f, Language: cfg.Language})
	events <- dictationEvent{text: text, err: err}
}

// Called only on the main UI loop, including editor insertion.
func (i *Interactive) refreshDictation() {
	i.mu.Lock()
	defer i.mu.Unlock()
	for i.dictation.events != nil {
		select {
		case event := <-i.dictation.events:
			if event.transcribing {
				i.dictation.phase = "transcribing"
				i.invalidate()
				continue
			}
			i.dictation.cancel()
			<-i.dictation.done
			i.dictation = interactiveDictation{}
			if event.err != nil {
				i.statusErr = dictationError(event.err)
				i.statusOK = ""
			} else {
				text := strings.TrimSpace(event.text)
				if text == "" {
					i.statusErr = "No speech detected"
				} else {
					line := []rune(i.ed.Lines[i.ed.CursorR])
					if i.ed.CursorC > 0 && !unicode.IsSpace(line[i.ed.CursorC-1]) {
						text = " " + text
					}
					i.ed.Insert(text)
				}
			}
			i.invalidate()
		default:
			return
		}
	}
}

func (i *Interactive) cancelDictation() {
	i.mu.Lock()
	state := i.dictation
	i.dictation = interactiveDictation{}
	i.mu.Unlock()
	if state.cancel != nil {
		state.cancel()
		<-state.done
	}
	i.invalidate()
}

func dictationError(err error) string {
	switch {
	case errors.Is(err, ErrDictationLogin):
		return "Dictation needs a ChatGPT login (/login)"
	case errors.Is(err, provider.ErrTranscribeUnauthorized):
		return "ChatGPT login expired, run /login"
	case errors.Is(err, provider.ErrTranscribeEmpty):
		return "No speech detected"
	case errors.Is(err, context.DeadlineExceeded):
		return "Dictation timed out, try again"
	}
	// Provider errors are already sanitized, but never display arbitrary server
	// payloads or errors from injected credential sources.
	if strings.HasPrefix(err.Error(), "dictation:") || strings.HasPrefix(err.Error(), "dictation command") {
		return truncateResidentSubagentIndicator(sanitizeSessionTreeText(err.Error()), 180)
	}
	return "Dictation failed, try again"
}

func (i *Interactive) dictationIndicatorLocked(width int) []string {
	state := i.dictation
	if state.phase == "" || width <= 0 {
		return nil
	}
	now := i.clock()
	color := i.cfg.Theme.Spinner
	text := i.spin.FrameAt(now) + " Transcribing…"
	if state.phase == "recording" {
		seconds := int(now.Sub(state.started) / time.Second)
		dot := "●"
		if now.UnixMilli()/480%2 != 0 {
			dot = " "
		}
		text = fmt.Sprintf("%s REC %02d:%02d  Alt+V stop · Esc cancel", dot, seconds/60, seconds%60)
		color = i.cfg.Theme.Error
	}
	return []string{i.cfg.Theme.FGColor(color, truncateResidentSubagentIndicator(text, width))}
}
