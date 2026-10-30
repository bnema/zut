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
	"github.com/mattn/go-runewidth"
)

// ErrDictationLogin means no ChatGPT OAuth credential is available.
var ErrDictationLogin = errors.New("dictation needs login")

// DictationRecording is the lifecycle owned by one interactive dictation.
type DictationRecording interface {
	Stop() (dictation.Result, error)
	Cancel()
	Done() <-chan struct{}
	Level() float64
	Recorder() string
}

type DictationConfig struct {
	Transcriber provider.Transcriber
	Recorder    dictation.Options
	Language    string
	Service     string
	// Start overrides recorder creation for embedded hosts and tests.
	Start func(context.Context, dictation.Options) (DictationRecording, error)
}

type dictationEvent struct {
	recording    DictationRecording
	audio        dictation.Result
	size         int64
	duration     time.Duration
	transcribing bool
	text         string
	err          error
}

// All state belongs to the UI loop and is protected by i.mu for rendering.
// Workers publish immutable events; they never access the editor or UI state.
type interactiveDictation struct {
	recording DictationRecording
	history   []rune
	audio     dictation.Result
	size      int64
	phase     string
	started   time.Time
	stop      chan struct{}
	events    <-chan dictationEvent
	cancel    context.CancelFunc
	done      <-chan struct{}
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
	events := make(chan dictationEvent, 4)
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
	events <- dictationEvent{recording: recording}
	select {
	case <-ctx.Done():
		return
	case <-stop:
	case <-recording.Done():
	}
	if ctx.Err() != nil {
		return
	}
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
		events <- dictationEvent{err: errors.New("dictation: could not read recorded audio")}
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	var size int64
	if info != nil {
		size = info.Size()
	}
	events <- dictationEvent{transcribing: true, audio: audio, size: size}
	requestCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	began := time.Now()
	text, err := cfg.Transcriber.Transcribe(requestCtx, provider.AudioInput{Filename: audio.Filename, MIMEType: audio.MIMEType, Reader: f, Language: cfg.Language})
	events <- dictationEvent{text: text, err: err, duration: time.Since(began)}
}

// Called only on the main UI loop, including editor insertion.
func (i *Interactive) refreshDictation() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.dictation.phase == "recording" && i.dictation.recording != nil {
		level := i.dictation.recording.Level()
		cell := []rune("▁▂▃▄▅▆▇█")[int(min(1, max(0, level))*7)]
		if len(i.dictation.history) == 0 {
			i.dictation.history = []rune("▁▁▁▁▁▁▁▁")
		}
		i.dictation.history = append(i.dictation.history[1:], cell)
	}
	for i.dictation.events != nil {
		select {
		case event := <-i.dictation.events:
			if event.recording != nil {
				i.dictation.recording = event.recording
				continue
			}
			if event.transcribing {
				i.dictation.audio = event.audio
				i.dictation.size = event.size
				i.dictation.phase = "transcribing"
				i.invalidate()
				continue
			}
			i.dictation.cancel()
			i.trackDictationCleanupLocked(i.dictation.done)
			i.dictation = interactiveDictation{}
			if event.err != nil {
				i.statusErr = dictationError(event.err, i.cfg.Dictation.Service)
				i.statusOK = ""
			} else {
				text := strings.TrimSpace(tui.SanitizePlainText(event.text))
				if text == "" {
					i.statusOK = ""
					i.statusErr = "No speech detected"
				} else {
					// The transcript is a draft in every mode; the user reviews
					// and sends it.
					line := []rune(i.ed.Lines[i.ed.CursorR])
					if i.ed.CursorC > 0 && !unicode.IsSpace(line[i.ed.CursorC-1]) {
						text = " " + text
					}
					i.ed.Insert(text)
					i.statusErr = ""
					i.statusOK = fmt.Sprintf("%d words · %s · %.1f s", len(strings.Fields(text)), i.cfg.Dictation.Service, event.duration.Seconds())
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
	if state.done != nil {
		i.trackDictationCleanupLocked(state.done)
	}
	i.mu.Unlock()
	if state.cancel != nil {
		state.cancel()
	}
	i.invalidate()
}

// trackDictationCleanupLocked retains only workers still cleaning up.
func (i *Interactive) trackDictationCleanupLocked(done <-chan struct{}) {
	pending := i.pendingDictation[:0]
	for _, ch := range i.pendingDictation {
		select {
		case <-ch:
		default:
			pending = append(pending, ch)
		}
	}
	i.pendingDictation = append(pending, done)
}

// shutdownDictation joins cleanup only on Run exit, never on the UI loop.
func (i *Interactive) shutdownDictation() {
	i.cancelDictation()
	i.mu.Lock()
	pending := i.pendingDictation
	i.pendingDictation = nil
	i.mu.Unlock()
	for _, done := range pending {
		<-done
	}
}

func dictationError(err error, service string) string {
	var recorder *dictation.RecorderError
	var httpErr *provider.TranscribeHTTPError
	switch {
	case errors.Is(err, dictation.ErrNoRecorder):
		return "No recorder found: install pw-record, parecord or ffmpeg"
	case errors.Is(err, dictation.ErrOutputPlaceholder):
		return "dictation.recorder needs an {output} placeholder"
	case errors.Is(err, dictation.ErrTooLittleAudio):
		return "Recording too short or microphone silent"
	case errors.Is(err, dictation.ErrFinalize):
		return "Recording could not be finalized"
	case errors.As(err, &recorder):
		text := "Recording failed (" + sanitizeSessionTreeText(recorder.Recorder) + ")"
		if recorder.Hint != "" {
			text += ": " + truncateResidentSubagentIndicator(sanitizeSessionTreeText(recorder.Hint), 100)
		}
		return text
	case errors.As(err, &httpErr):
		return fmt.Sprintf("%s transcription failed (HTTP %d)", service, httpErr.StatusCode)
	case errors.Is(err, provider.ErrTranscribeNetwork):
		return service + " transcription failed: network error"
	case errors.Is(err, provider.ErrTranscribeResponse):
		return service + " transcription failed: unexpected response"
	}

	switch {
	case errors.Is(err, ErrDictationLogin):
		return "Dictation needs a ChatGPT login (/login)"
	case errors.Is(err, provider.ErrTranscribeUnauthorized):
		return "ChatGPT login expired, run /login"
	case errors.Is(err, provider.ErrTranscribeEmpty):
		return "No speech detected"
	case errors.Is(err, context.DeadlineExceeded):
		return "Transcription timed out"
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
	size := fmt.Sprintf("%.0f KB", float64(state.size)/1024)
	if state.size >= 1024*1024 {
		size = fmt.Sprintf("%.1f MB", float64(state.size)/(1024*1024))
	}
	text := fmt.Sprintf("%s Transcribing %.0f s · %s → %s", i.spin.FrameAt(now), state.audio.Duration.Seconds(), size, i.cfg.Dictation.Service)
	if state.phase == "recording" {
		seconds := int(now.Sub(state.started) / time.Second)
		name := ""
		if state.recording != nil {
			name = sanitizeSessionTreeText(state.recording.Recorder())
		}
		base := fmt.Sprintf("● REC %02d:%02d · %s", seconds/60, seconds%60, name)
		history := string(state.history)
		if history == "" {
			history = "▁▁▁▁▁▁▁▁"
		}
		text = base + " " + history + "  Alt+V stop · Esc cancel"
		if runewidth.StringWidth(text) > width {
			text = base + " " + history
		}
		if runewidth.StringWidth(text) > width {
			text = base
		}

		color = i.cfg.Theme.Error
	}
	return []string{i.cfg.Theme.FGColor(color, truncateResidentSubagentIndicator(text, width))}
}
