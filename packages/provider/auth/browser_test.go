package auth

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"runtime"
	"testing"
)

func TestBrowserHelperProcess(t *testing.T) {
	if os.Getenv("ZUT_BROWSER_HELPER") != "1" {
		return
	}
	os.Stdout.Write([]byte("started\n"))
	var b [1]byte
	if _, err := os.Stdin.Read(b[:]); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserStartDoesNotWaitAndReaps(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestBrowserHelperProcess$")
	cmd.Env = append(os.Environ(), "ZUT_BROWSER_HELPER=1")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	done, err := startBrowser(cmd)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "started\n" {
		t.Fatal(line, err)
	}
	select {
	case <-done:
		t.Fatal("browser exited before release")
	default:
	}
	if _, err := in.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	in.Close()
	io.Copy(io.Discard, out)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatal("browser not reaped")
	}
}

func TestBrowserEventStillEmittedOnLaunchFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows system opener is outside PATH")
	}
	t.Setenv("PATH", t.TempDir())
	if err := OpenBrowser("https://example.invalid"); err == nil {
		t.Fatal("expected start failure")
	}
	m := NewManager(nil)
	m.maybeOpen("https://example.invalid")
	select {
	case e := <-m.Events():
		if e.Kind != "browser_open" || e.URL != "https://example.invalid" {
			t.Fatal(e)
		}
	default:
		t.Fatal("missing browser event")
	}
}
