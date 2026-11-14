//go:build !windows

package tools

import (
	"context"
	"io"
	"os"
	"testing"
	"time"
)

func TestRunPythonChildFiles(t *testing.T) {
	py := realPython3(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	outcome, err := runPythonProcess(ctx, context.Background(), pythonRunSpec{
		Exe: py, CWD: t.TempDir(), Env: os.Environ(), ChildFiles: []*os.File{w},
		Code: `import os; os.write(int(os.environ["ZUT_CODEMODE_FDS"]), b"ok")`,
	})
	// The runner waits for exit; this tiny write fits in the pipe buffer.
	// Close the parent's copy too, so ReadAll observes EOF.
	if closeErr := w.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ExitCode != 0 || outcome.TimedOut || outcome.Cancelled {
		t.Fatalf("child failed: %+v", outcome)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ok" {
		t.Fatalf("child pipe = %q, want ok", got)
	}
}

func TestRunPythonChildFilesTwoDescriptors(t *testing.T) {
	py := realPython3(t)
	var readers, writers []*os.File
	for range 2 {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close(); w.Close() })
		readers = append(readers, r)
		writers = append(writers, w)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	outcome, err := runPythonProcess(ctx, context.Background(), pythonRunSpec{
		Exe: py, CWD: t.TempDir(), Env: os.Environ(), ChildFiles: writers,
		Code: `import os
assert os.environ["ZUT_CODEMODE_FDS"] == "3,4"
os.write(3, b"first")
os.write(4, b"second")`,
	})
	for _, w := range writers {
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ExitCode != 0 || outcome.TimedOut || outcome.Cancelled {
		t.Fatalf("child failed: %+v", outcome)
	}
	for i, want := range []string{"first", "second"} {
		if err := readers[i].SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(readers[i])
		if err != nil || string(got) != want {
			t.Fatalf("pipe %d = %q, %v; want %q", i, got, err, want)
		}
	}
}

func TestRunPythonExplicitEnvPrecedence(t *testing.T) {
	py := realPython3(t)
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"explicit", []string{"ZUT_RUNNER_ENV=explicit"}, "explicit\n"},
		{"empty", []string{}, "absent\n"},
		{"nil", nil, "context\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(withToolEnv(context.Background(), []string{"ZUT_RUNNER_ENV=context"}), 15*time.Second)
			defer cancel()
			outcome, err := runPythonProcess(ctx, context.Background(), pythonRunSpec{
				Exe: py, CWD: t.TempDir(), Env: tc.env,
				Code: `import os; print(os.environ.get("ZUT_RUNNER_ENV", "absent"))`,
			})
			if err != nil {
				t.Fatal(err)
			}
			if outcome.ExitCode != 0 || outcome.TimedOut || outcome.Cancelled || outcome.Captured != tc.want {
				t.Fatalf("outcome = %+v, want output %q", outcome, tc.want)
			}
		})
	}
}

func TestRunPythonChildFilesCancellation(t *testing.T) {
	py := realPython3(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	// The bridge readiness byte synchronizes cancellation after inheritance.
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, deadlineCancel := context.WithTimeout(parent, 15*time.Second)
	defer deadlineCancel()
	type result struct {
		outcome pythonExecOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := runPythonProcess(ctx, parent, pythonRunSpec{
			Exe: py, CWD: "", Env: os.Environ(), ChildFiles: []*os.File{w},
			Code: `import os, time; os.write(int(os.environ["ZUT_CODEMODE_FDS"]), b"!"); time.sleep(60)`,
		})
		done <- result{outcome, err}
	}()
	if err := r.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var ready [1]byte
	if _, err := io.ReadFull(r, ready[:]); err != nil {
		t.Fatal(err)
	}
	if ready[0] != '!' {
		t.Fatalf("readiness = %q", ready)
	}
	cancel()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !got.outcome.Cancelled {
			t.Fatalf("not cancelled: %+v", got.outcome)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runner did not return after cancellation")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(r)
	if err != nil || len(rest) != 0 {
		t.Fatalf("want EOF after closing parent copy, got %q, %v", rest, err)
	}
}
