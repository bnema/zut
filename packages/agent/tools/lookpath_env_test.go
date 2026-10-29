package tools

import (
	"os"
	"strings"
	"testing"
)

func TestLookPathEnvFailsClosed(t *testing.T) {
	if _, err := lookPathEnv("rel", []string{"PATH=/bin"}, "sh"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative cwd: %v", err)
	}
	t.Setenv("PATH", os.Getenv("PATH"))
	if got, err := lookPathEnv(t.TempDir(), nil, "go"); err == nil {
		t.Fatalf("nil env resolved %q from process PATH", got)
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
