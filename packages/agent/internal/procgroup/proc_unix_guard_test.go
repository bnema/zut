//go:build !windows

package procgroup

import (
	"github.com/stretchr/testify/mock"
	"os/exec"
	"syscall"
	"testing"
)

func TestGuardForgetsGroupOnceGone(t *testing.T) {
	k := newMockSignaler(t)
	k.EXPECT().Kill(-4242, syscall.Signal(0)).RunAndReturn(func(int, syscall.Signal) error { return syscall.ESRCH }).Once()
	g := &Guard{pgid: 4242, sender: k}
	if g.alive() {
		t.Fatal("ESRCH means the group is gone")
	}
	g.Terminate(false)
	g.Terminate(true)
	if g.alive() {
		t.Fatal("alive after gone")
	}
	g.Cleanup()
}

func TestGuardCleanupIsOneShot(t *testing.T) {
	k := newMockSignaler(t)
	var sigs []syscall.Signal
	k.EXPECT().Kill(-4243, mock.Anything).RunAndReturn(func(_ int, sig syscall.Signal) error {
		sigs = append(sigs, sig)
		return nil
	})
	g := &Guard{pgid: 4243, sender: k}
	g.Cleanup()
	after := len(sigs)
	if after == 0 {
		t.Fatal("cleanup sent nothing")
	}
	for i := 0; i < 3; i++ {
		g.Cleanup()
		g.Terminate(true)
		g.alive()
	}
	if len(sigs) != after {
		t.Fatal("signals after cleanup")
	}
	var signals []syscall.Signal
	for _, sig := range sigs {
		if sig != 0 {
			signals = append(signals, sig)
		}
	}
	if len(signals) != 2 || signals[0] != syscall.SIGTERM || signals[1] != syscall.SIGKILL {
		t.Fatalf("signal order = %v", signals)
	}
	if g.pgid != 0 {
		t.Fatal("group retained after cleanup")
	}
}

func TestGuardNoSignalWhenAlreadyEmpty(t *testing.T) {
	k := newMockSignaler(t)
	k.EXPECT().Kill(-4244, syscall.Signal(0)).RunAndReturn(func(int, syscall.Signal) error { return syscall.ESRCH }).Once()
	g := &Guard{pgid: 4244, sender: k}
	g.Cleanup()
}

// The second non-reaping wait must still observe our child after Cleanup.
// This runs on BSD/macOS as well as Linux, exercising kqueue there.
func TestCleanupReservesLeaderUntilCallerWaits(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	Isolate(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	g, err := New(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !g.AwaitLeaderExit() {
		t.Fatal("non-reaping wait failed")
	}
	g.Cleanup()
	if err := waitLeaderExited(cmd.Process.Pid); err != nil {
		t.Fatalf("leader reaped before caller Wait: %v", err)
	}
	if g.pgid != 0 {
		t.Fatal("guard retained signalable group after cleanup")
	}
}

func TestRepeatedCleanupDoesNotSignalAfterReap(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	Isolate(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g, err := New(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if g.AwaitLeaderExit() {
		g.Cleanup()
	}
	_ = cmd.Wait()
	k := newMockSignaler(t)
	g.sender = k
	g.Cleanup()
	g.Terminate(true)
}
