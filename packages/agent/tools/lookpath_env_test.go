package tools

import (
	"strings"
	"testing"
)

func TestEnvIsolationLookPathEnvOS(t *testing.T) {
	set := func(paths ...string) func(string, string) bool {
		return func(_, p string) bool {
			for _, want := range paths {
				if p == want {
					return true
				}
			}
			return false
		}
	}
	cases := []struct {
		name, goos, cwd, exe, want string
		env                        []string
		exists                     []string
		wantErr                    string
	}{
		{name: "unix path", goos: "linux", cwd: "/w", exe: "rg", env: []string{"PATH=/a:/b"}, exists: []string{"/b/rg"}, want: "/b/rg"},
		{name: "unix relative entry anchored at cwd", goos: "linux", cwd: "/w", exe: "rg", env: []string{"PATH=bin"}, exists: []string{"/w/bin/rg"}, want: "/w/bin/rg"},
		{name: "unix backslash is not a separator", goos: "linux", cwd: "/w", exe: `a\rg`, env: []string{"PATH=/a"}, exists: []string{`/a/a\rg`}, want: `/a/a\rg`},
		{name: "unix slash name anchored at cwd", goos: "linux", cwd: "/w", exe: "./x", env: []string{"PATH=/a"}, exists: []string{"/w/./x"}, want: "/w/./x"},
		{name: "empty PATH", goos: "linux", cwd: "/w", exe: "rg", env: []string{"PATH="}, exists: []string{"/w/rg"}, wantErr: "not found"},
		{name: "missing PATH", goos: "linux", cwd: "/w", exe: "rg", env: []string{}, exists: []string{"/w/rg"}, wantErr: "not found"},
		{name: "relative cwd", goos: "linux", cwd: "w", exe: "rg", env: []string{"PATH=/a"}, wantErr: "absolute"},
		{name: "windows pathext from env", goos: "windows", cwd: `C:\w`, exe: "git", env: []string{`Path=C:\g`, "PATHEXT=.PS1;.EXE"}, exists: []string{`C:\g\git.EXE`}, want: `C:\g\git.EXE`},
		{name: "windows dotted name gets ext", goos: "windows", cwd: `C:\w`, exe: "python3.12", env: []string{`PATH=C:\p`, "PATHEXT=.EXE"}, exists: []string{`C:\p\python3.12.EXE`}, want: `C:\p\python3.12.EXE`},
		{name: "windows name with ext as-is", goos: "windows", cwd: `C:\w`, exe: "cmd.exe", env: []string{`PATH=C:\s`, "PATHEXT=.EXE"}, exists: []string{`C:\s\cmd.exe`}, want: `C:\s\cmd.exe`},
		{name: "windows relative entry", goos: "windows", cwd: `C:\w`, exe: "rg", env: []string{`PATH=tools`, "PATHEXT=.EXE"}, exists: []string{`C:\w\tools\rg.EXE`}, want: `C:\w\tools\rg.EXE`},
		{name: "windows backslash name", goos: "windows", cwd: `C:\w`, exe: `bin\rg.exe`, env: []string{`PATH=C:\x`, "PATHEXT=.EXE"}, exists: []string{`C:\w\bin\rg.exe`}, want: `C:\w\bin\rg.exe`},
		{name: "windows drive-relative cwd", goos: "windows", cwd: `C:project`, exe: "rg", env: []string{`PATH=C:\x`}, wantErr: "absolute"},
		{name: "windows drive-relative entry anchored", goos: "windows", cwd: `C:\w`, exe: "rg", env: []string{`PATH=D:t`, "PATHEXT=.EXE"}, exists: []string{`C:\w\t\rg.EXE`}, want: `C:\w\t\rg.EXE`},
		{name: "windows empty PATH", goos: "windows", cwd: `C:\w`, exe: "rg", env: []string{"PATHEXT=.EXE"}, exists: []string{`C:\w\rg.EXE`}, wantErr: "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := lookPathEnvOS(tc.goos, tc.cwd, tc.env, tc.exe, set(tc.exists...))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %q err %v, want error %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q err %v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestPerSessionWindowsDriveRelativeCWD(t *testing.T) {
	for p, want := range map[string]bool{`C:project`: false, `C:`: false, `\x`: false, `C:\x`: true, `c:/x`: true, `\\srv\share`: true, `rel`: false} {
		if got := isAbsSessionPath("windows", p); got != want {
			t.Fatalf("%q: got %v want %v", p, got, want)
		}
	}
	for p, want := range map[string]bool{`/x`: true, `C:\x`: false, `.`: false} {
		if got := isAbsSessionPath("linux", p); got != want {
			t.Fatalf("%q: got %v want %v", p, got, want)
		}
	}
}
