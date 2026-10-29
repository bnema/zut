package execpath

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSessionPATH(t *testing.T) {
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	name, ext := "l2-command", ""
	if runtime.GOOS == "windows" {
		ext = ".custom"
	}
	want := filepath.Join(cwd, "bin", name+ext)
	if err := os.WriteFile(want, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(name, cwd, []string{"PATH=bin", "PATHEXT=.custom"})
	if err != nil || got != want {
		t.Fatalf("Resolve = %q, %v; want %q", got, err, want)
	}
	for _, env := range [][]string{{}, {"PATH="}} {
		if _, err := Resolve(name, cwd, env); err == nil {
			t.Fatal("empty session PATH resolved executable")
		}
	}
}
