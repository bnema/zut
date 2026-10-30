package extensions

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestExtensionShutdownHelper(t *testing.T) {
	if os.Getenv("ZUT_TEST_SHUTDOWN_HELPER") != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	_, _ = os.Stdout.Write([]byte("ready"))
	// Bound the helper lifetime even if the parent test fails.
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

func TestStopExtensionsReapsUnresponsiveChildrenConcurrently(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires SIGTERM handling")
	}
	var exts []*Extension
	for n := 0; n < 3; n++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestExtensionShutdownHelper$")
		cmd.Env = append(os.Environ(), "ZUT_TEST_SHUTDOWN_HELPER=1")
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = out.Close() })
		if _, err := io.ReadFull(out, make([]byte, len("ready"))); err != nil {
			t.Fatal(err)
		}
		exts = append(exts, &Extension{cmd: cmd})
	}
	start := time.Now()
	stopExtensions(exts, 20*time.Millisecond)
	elapsed := time.Since(start)
	t.Logf("closed %d unresponsive extensions in %s", len(exts), elapsed)
	for _, ext := range exts {
		if ext.cmd.ProcessState == nil {
			t.Fatal("shutdown returned without reaping a child")
		}
	}
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("shutdown took %s; termination grace periods must overlap", elapsed)
	}
}
