package execpath

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestUnixBackslashNameUsesSessionPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backslash is a separator on Windows")
	}
	root := t.TempDir()
	host, session := filepath.Join(root, "host"), filepath.Join(root, "session")
	name := `server\name`
	for _, dir := range []string{host, session} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", host)
	want := filepath.Join(session, name)
	got, err := Resolve(name, root, []string{"PATH=" + session})
	if err != nil || got != want {
		t.Fatalf("Resolve = %q, %v; want %q", got, err, want)
	}
	if got, err := Resolve(name, root, []string{}); err == nil {
		t.Fatalf("empty session env resolved %q", got)
	}
}

func TestWindowsRules(t *testing.T) {
	files := map[string]bool{
		`C:\tools\server.cmd`:    true,
		`C:\work\server.custom`:  true,
		`C:\bin\server.v1.exe`:   true,
		`C:\bin\tool.bat`:        true,
		`D:\other\x.exe`:         true,
		`C:\root.exe`:            true,
		`\\host\share\b\srv.exe`: true,
		`C:\work\rel\here.exe`:   true,
		`C:\sess\dotted.v2`:      true,
		`C:\sess\dotted.v2.exe`:  true,
	}
	isExecutable := func(name string) bool { return files[name] }
	tests := []struct {
		name, cmd, cwd string
		env            []string
		want           string
	}{
		{"explicit absolute uses session PATHEXT", `C:\tools\server`, `C:\work`, []string{"PATHEXT=.CMD"}, `C:\tools\server.cmd`},
		{"explicit absolute skips process PATHEXT", `C:\tools\server`, `C:\work`, []string{"PATHEXT=.EXE"}, ""},
		{"dot relative", `.\server`, `C:\work`, []string{"PATHEXT=custom;;"}, `C:\work\server.custom`},
		{"forward slash relative", `rel/here`, `C:\work`, []string{}, `C:\work\rel\here.exe`},
		{"rooted on cwd volume", `\root`, `C:\work`, []string{}, `C:\root.exe`},
		{"drive relative other drive", `D:x`, `C:\work`, []string{}, ""},
		{"UNC", `\\host\share\b\srv`, `C:\work`, []string{}, `\\host\share\b\srv.exe`},
		{"dotted bare falls back", "server.v1", `C:\work`, []string{`Path="C:\bin";C:\nope`}, `C:\bin\server.v1.exe`},
		{"dotted exact wins", "dotted.v2", `C:\work`, []string{`PATH=C:\sess`}, `C:\sess\dotted.v2`},
		{"case-insensitive env keys", "tool", `C:\work`, []string{`path=C:\bin`, `pathext=.BAT`}, `C:\bin\tool.bat`},
		{"relative PATH entry", "here", `C:\work`, []string{`PATH=rel`}, `C:\work\rel\here.exe`},
		{"empty env", "tool", `C:\work`, []string{}, ""},
		{"relative cwd rejected", `.\server`, `work`, []string{}, ""},
	}
	for _, tt := range tests {
		got, err := resolve(true, tt.cmd, tt.cwd, tt.env, isExecutable)
		if tt.want == "" {
			if err == nil {
				t.Errorf("%s: resolved %q, want error", tt.name, got)
			}
		} else if err != nil || got != tt.want {
			t.Errorf("%s: got %q, %v; want %q", tt.name, got, err, tt.want)
		}
	}
	if !hasSeparator(true, `a\b`) || hasSeparator(false, `a\b`) || !hasSeparator(false, "a/b") {
		t.Error("platform separator detection wrong")
	}
	if got := pathExts("EXE;;cmd"); strings.Join(got, ",") != ".exe,.cmd" {
		t.Errorf("pathExts = %v", got)
	}
}
