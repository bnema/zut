package flock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestTryLockExcludesSecondOwnerAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "x.lock")
	first, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(path); !errors.Is(err, ErrBusy) {
		t.Fatalf("second TryLock = %v, want ErrBusy", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	second, err := TryLock(path)
	if err != nil {
		t.Fatalf("TryLock after Close: %v", err)
	}
	defer second.Close()
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("lock mode = %v", fi.Mode().Perm())
		}
		di, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %v", di.Mode().Perm())
		}
	}
}

func TestTryLockExcludesOtherProcess(t *testing.T) {
	if os.Getenv("ZUT_FLOCK_HELPER") == "1" {
		l, err := TryLock(os.Getenv("ZUT_FLOCK_PATH"))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, "ready")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		_ = l.Close()
		return
	}
	path := filepath.Join(t.TempDir(), "x.lock")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTryLockExcludesOtherProcess$")
	cmd.Env = append(os.Environ(), "ZUT_FLOCK_HELPER=1", "ZUT_FLOCK_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("helper readiness = %q, %v", line, err)
	}
	if _, err := TryLock(path); !errors.Is(err, ErrBusy) {
		t.Fatalf("TryLock while other process holds = %v", err)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	l, err := TryLock(path)
	if err != nil {
		t.Fatalf("TryLock after owner exit: %v", err)
	}
	_ = l.Close()
}
