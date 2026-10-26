//go:build !windows

package dictation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func fake(t *testing.T, body string) Options {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "recorder")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return Options{Command: []string{script, "{output}"}, TempDir: dir}
}

const capturing = `trap 'exit 0' INT
# Write a fake RIFF/WAVE payload; no microphone access.
printf 'RIFF0000WAVE' > "$1"
dd if=/dev/zero bs=2048 count=1 >> "$1" 2>/dev/null
: > "$1.ready"
while :; do sleep 1; done
`

func ready(t *testing.T, r *Recording) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(r.result.Path + ".ready"); err == nil {
			return
		}
		select {
		case <-timer.C:
			t.Fatal("recorder not ready")
		case <-r.Done():
			t.Fatal("recorder exited before ready")
		case <-ticker.C:
		}
	}
}
func finished(t *testing.T, r *Recording) {
	t.Helper()
	select {
	case <-r.Done():
	case <-time.After(6 * time.Second):
		t.Fatal("recorder did not exit")
	}
}

// pw-record exits with status 1 after SIGINT even though the WAV is complete.
func TestStopAcceptsNonZeroExitAfterInterrupt(t *testing.T) {
	opts := fake(t, strings.Replace(capturing, "trap 'exit 0' INT", "trap 'exit 1' INT", 1))
	r, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Cancel)
	ready(t, r)
	result, err := r.Stop()
	if err != nil {
		t.Fatalf("stop after interrupt: %v", err)
	}
	if err := result.Cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestStop(t *testing.T) {
	r, err := Start(context.Background(), fake(t, capturing))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Cancel()
	ready(t, r)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				_ = r.Elapsed()
				_ = r.Done()
			}
		})
	}
	result, err := r.Stop()
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 1024 || info.Mode().Perm() != 0600 || result.MIMEType != "audio/wav" || !strings.HasSuffix(result.Filename, ".wav") || result.Duration <= 0 || result.Recorder != "recorder" {
		t.Fatalf("%+v %v", result, info)
	}
	again, err := r.Stop()
	if err != nil || again != result {
		t.Fatalf("%+v %v", again, err)
	}
	if r.Elapsed() != result.Duration {
		t.Fatal("elapsed not frozen")
	}
	if err = result.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if err = result.Cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestCancel(t *testing.T) {
	r, err := Start(context.Background(), fake(t, capturing))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Cancel)
	ready(t, r)
	r.Cancel()
	r.Cancel()
	if _, err = os.Stat(r.result.Path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err = r.Stop(); err == nil {
		t.Fatal("canceled stop succeeded")
	}
}

func TestContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := Start(ctx, fake(t, capturing))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Cancel()
	ready(t, r)
	cancel()
	finished(t, r)
	if _, err = os.Stat(r.result.Path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err = r.Stop(); err == nil {
		t.Fatal("expected cancellation")
	}
}

func TestMaxDuration(t *testing.T) {
	opts := fake(t, capturing)
	deadline := make(chan time.Time, 1)
	opts.limit = deadline
	r, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Cancel()
	ready(t, r)
	deadline <- time.Now()
	finished(t, r)
	result, err := r.Stop()
	if err != nil {
		t.Fatal(err)
	}
	if result.Duration <= 0 {
		t.Fatal(result.Duration)
	}
}

func TestFailures(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"small", "printf RIFF > \"$1\"\n", "too little audio"},
		{"stderr", "printf '\\033[31mdevice\\r\\nfailed\\007' >&2\nexit 1\n", "device failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Start(context.Background(), fake(t, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Cancel()
			finished(t, r)
			_, err = r.Stop()
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.ContainsAny(err.Error(), "\x1b\r\n\a") {
				t.Fatalf("%v", err)
			}
			if _, err = os.Stat(r.result.Path); !os.IsNotExist(err) {
				t.Fatal(err)
			}
		})
	}
}

func TestStartFailureCleanup(t *testing.T) {
	opts := Options{TempDir: t.TempDir(), Command: []string{"/does/not/exist", "{output}"}}
	if _, err := Start(context.Background(), opts); err == nil {
		t.Fatal("expected failure")
	}
	entries, err := os.ReadDir(opts.TempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("%v %v", entries, err)
	}
}

func TestTailBound(t *testing.T) {
	tail := &stderrTail{}
	_, _ = tail.Write([]byte(strings.Repeat("x", 1000)))
	_, _ = tail.Write([]byte("last"))
	if len(tail.String()) > 300 || !strings.HasSuffix(tail.String(), "last") {
		t.Fatal(tail.String())
	}
}

func TestForcedStop(t *testing.T) {
	opts := fake(t, "trap '' INT\n"+strings.Replace(capturing, "trap 'exit 0' INT\n", "", 1))
	r, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Cancel()
	ready(t, r)
	if _, err = r.Stop(); err == nil || !strings.Contains(err.Error(), "did not stop gracefully") {
		t.Fatal(err)
	}
	if _, err = os.Stat(r.result.Path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestConcurrentStopCancel(t *testing.T) {
	r, err := Start(context.Background(), fake(t, capturing))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Cancel)
	ready(t, r)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { _, _ = r.Stop() })
		wg.Go(func() { r.Cancel() })
	}
	wg.Wait()
	if _, err = os.Stat(r.result.Path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestLevelEndsWithRecording(t *testing.T) {
	loud := strings.Replace(capturing, "dd if=/dev/zero bs=2048 count=1 >> \"$1\" 2>/dev/null", "i=0; while [ $i -lt 1024 ]; do printf '\\377\\177\\000\\200'; i=$((i+1)); done >> \"$1\"", 1)
	r, err := Start(context.Background(), fake(t, loud))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Cancel)
	ready(t, r)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for r.Level() == 0 {
		select {
		case <-deadline.C:
			t.Fatal("recorder never sampled nonzero PCM")
		case <-r.Done():
			t.Fatal("recorder exited before sampling")
		case <-tick.C:
		}
	}
	r.Cancel()
	finished(t, r)
	for j := 0; j < 100; j++ {
		if r.Level() != 0 {
			t.Fatal(r.Level())
		}
	}
}

func TestCrashBeforeRequestedStop(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprint("limit=", automatic), func(t *testing.T) {
			fifo := filepath.Join(t.TempDir(), "release")
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			opts := fake(t, "dd if=/dev/zero bs=2048 count=1 > \"$1\" 2>/dev/null\nread release < '"+fifo+"'\nexit 7\n")
			opts.beforeStop = func(exited chan error) {
				f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
				if err != nil {
					panic(err)
				}
				_, _ = f.WriteString("exit\n")
				_ = f.Close()
				// Wait has completed; leave its result queued for the supervisor check.
				exit := <-exited
				exited <- exit
			}
			limit := make(chan time.Time, 1)
			opts.limit = limit
			r, err := Start(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(r.Cancel)
			if automatic {
				limit <- time.Now()
				finished(t, r)
			}
			_, err = r.Stop()
			var recorderErr *RecorderError
			if !errors.As(err, &recorderErr) {
				t.Fatalf("expected RecorderError, got %v", err)
			}
		})
	}
}
