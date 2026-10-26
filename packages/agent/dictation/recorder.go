// Package dictation records private WAV files using an external recorder.
// It does not access audio devices itself. Command overrides must produce WAV
// at {output}, preserve its permissions, and support the platform stop signal.
package dictation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Options configures a single recording. Zero MaxDuration means no time limit.
type Options struct {
	Command     []string
	MaxDuration time.Duration
	TempDir     string
	LookPath    func(string) (string, error)
}

// Result owns the recorded file; the caller must call Cleanup after use.
type Result struct {
	Path     string
	Filename string
	MIMEType string
	Duration time.Duration
	Recorder string
}

// Cleanup removes the audio file. It is safe to call repeatedly.
func (r Result) Cleanup() error {
	err := os.Remove(r.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Recording supervises one recorder. All its methods are safe concurrently.
type Recording struct {
	mu         sync.Mutex
	result     Result
	err        error
	started    time.Time
	finished   time.Time
	stop       chan struct{}
	cancel     chan struct{}
	done       chan struct{}
	stopOnce   sync.Once
	cancelOnce sync.Once
}

func command(opts Options, output, platform string) ([]string, string, error) {
	lookup := opts.LookPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	if len(opts.Command) > 0 {
		argv := append([]string(nil), opts.Command...)
		found := false
		for i, arg := range argv {
			if strings.Contains(arg, "{output}") {
				found = true
			}
			argv[i] = strings.ReplaceAll(arg, "{output}", output)
		}
		if !found {
			return nil, "", errors.New("dictation command requires an {output} placeholder")
		}
		return argv, filepath.Base(argv[0]), nil
	}
	for _, name := range []string{"pw-record", "parecord", "ffmpeg"} {
		path, err := lookup(name)
		if err != nil {
			continue
		}
		argv := []string{path}
		switch name {
		case "pw-record":
			argv = append(argv, "--rate", "16000", "--channels", "1", "--format", "s16", "--container", "wav", output)
		case "parecord":
			argv = append(argv, "--rate=16000", "--channels=1", "--format=s16le", "--file-format=wav", output)
		case "ffmpeg":
			argv = append(argv, "-loglevel", "error")
			switch platform {
			case "linux":
				argv = append(argv, "-nostdin", "-f", "pulse", "-i", "default")
			case "darwin":
				argv = append(argv, "-nostdin", "-f", "avfoundation", "-i", ":0")
			case "windows":
				return nil, "", errors.New("dictation: ffmpeg has no reliable default Windows microphone; configure Command with -f dshow -i audio=<device> and {output}")
			default:
				return nil, "", fmt.Errorf("dictation: unsupported ffmpeg input platform %s", platform)
			}
			argv = append(argv, "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-y", output)
		}
		return argv, name, nil
	}
	return nil, "", errors.New("dictation: no recorder found on PATH (tried pw-record, parecord, ffmpeg)")
}

// Start creates a private temporary WAV and starts a recorder without waiting
// for device initialization. Initialization failures are reported by Stop.
func Start(ctx context.Context, opts Options) (*Recording, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.MaxDuration < 0 {
		return nil, errors.New("dictation: negative MaxDuration")
	}
	f, err := os.CreateTemp(opts.TempDir, "zut-dictation-*.wav")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	if err = f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	argv, name, err := command(opts, path, runtime.GOOS)
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	isolate(cmd)
	cmd.WaitDelay = 3 * time.Second
	tail := &stderrTail{}
	cmd.Stderr = tail
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("dictation: start %s: %w", name, err)
	}
	r := &Recording{result: Result{Path: path, Filename: filepath.Base(path), MIMEType: "audio/wav", Recorder: name}, started: time.Now(), stop: make(chan struct{}), cancel: make(chan struct{}), done: make(chan struct{})}
	go r.run(ctx, cmd, stdin, tail, opts.MaxDuration)
	return r, nil
}

func (r *Recording) run(ctx context.Context, cmd *exec.Cmd, stdin io.WriteCloser, tail *stderrTail, maximum time.Duration) {
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	var limit <-chan time.Time
	if maximum > 0 {
		timer := time.NewTimer(maximum)
		defer timer.Stop()
		limit = timer.C
	}
	var err error
	canceled, stopping, forced := false, false, false
	select {
	case err = <-exited:
	case <-ctx.Done():
		canceled = true
	case <-r.cancel:
		canceled = true
	case <-r.stop:
		stopping = true
	case <-limit:
		stopping = true
	}
	if canceled {
		killTree(cmd)
		err = <-exited
	} else if stopping {
		graceful(cmd, stdin, r.result.Recorder)
		timer := time.NewTimer(3 * time.Second)
		select {
		case err = <-exited:
		case <-ctx.Done():
			canceled = true
			killTree(cmd)
			err = <-exited
		case <-r.cancel:
			canceled = true
			killTree(cmd)
			err = <-exited
		case <-timer.C:
			forced = true
			killTree(cmd)
			err = <-exited
		}
		timer.Stop()
	}
	// Also reap descendants that outlive the recorder.
	killTree(cmd)
	_ = stdin.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = time.Now()
	r.result.Duration = r.finished.Sub(r.started)
	if canceled || ctx.Err() != nil {
		r.err = errors.New("dictation: recording canceled")
	} else {
		info, statErr := os.Stat(r.result.Path)
		switch {
		case forced:
			r.err = errors.New("dictation: recorder did not stop gracefully")
		case err != nil && !(stopping && expectedStop(err, r.result.Recorder)):
			r.err = fmt.Errorf("dictation: %s failed: %w", r.result.Recorder, err)
		case statErr != nil:
			r.err = statErr
		case info.Size() < 1024:
			r.err = errors.New("dictation: recorder produced too little audio (less than 1 KB)")
		}
	}
	if r.err != nil {
		if diagnostic := tail.String(); diagnostic != "" {
			r.err = fmt.Errorf("%w: %s", r.err, diagnostic)
		}
		_ = r.result.Cleanup()
	}
	close(r.done)
}

// Stop finalizes the WAV, waiting at most the graceful-stop timeout plus process
// cleanup. It returns the same result on repeated calls (unless canceled).
func (r *Recording) Stop() (Result, error) {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return Result{}, r.err
	}
	return r.result, nil
}

// Cancel kills the recorder and deletes its file, including an already stopped file.
func (r *Recording) Cancel() {
	r.cancelOnce.Do(func() { close(r.cancel) })
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.result.Cleanup()
	r.err = errors.New("dictation: recording canceled")
}

// Done closes after process exit and finalization, including automatic stops.
func (r *Recording) Done() <-chan struct{} { return r.done }

// Elapsed returns recording wall-clock time, frozen after process exit.
func (r *Recording) Elapsed() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished.IsZero() {
		return r.result.Duration
	}
	return time.Since(r.started)
}

// stderrTail retains bounded diagnostics, never the recorder's full output.
type stderrTail struct{ data []byte }

func (t *stderrTail) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= 300 {
		t.data = append(t.data[:0], p[len(p)-300:]...)
	} else {
		t.data = append(t.data, p...)
		if len(t.data) > 300 {
			t.data = t.data[len(t.data)-300:]
		}
	}
	return n, nil
}
func (t *stderrTail) String() string {
	// Strip terminal controls and non-ASCII bytes, including ANSI escape bytes.
	s := strings.Map(func(r rune) rune {
		if r > 126 || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, string(t.data))
	return strings.Join(strings.Fields(s), " ")
}
