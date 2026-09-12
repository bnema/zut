package extensions

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadErrorsIdentifyExtensionDirectory(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		mode := "discover"
		if explicit {
			mode = "explicit"
		}
		for _, tc := range []struct {
			name     string
			manifest string
			want     string
		}{
			{"invalid-json", `{`, "parse manifest"},
			{"missing-name", `{"exec":"missing"}`, "name is required"},
			{"missing-exec", `{"name":"broken"}`, "exec is required"},
			{"open-log", `{"name":"broken","exec":"missing"}`, "open log"},
			{"missing-runtime", `{"name":"broken","exec":"zut-test-missing-runtime"}`, "failed to start"},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				home := t.TempDir()
				dir := filepath.Join(home, "extensions", tc.name)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "extension.json"), []byte(tc.manifest), 0o644); err != nil {
					t.Fatal(err)
				}
				if tc.name == "open-log" {
					if err := os.WriteFile(filepath.Join(home, "logs"), nil, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				if tc.name == "missing-runtime" {
					t.Setenv("PATH", t.TempDir())
				}
				mgr := New(home, "", "test", "", "", nil)
				var errs []error
				if explicit {
					errs = mgr.LoadExplicit(context.Background(), []string{dir})
				} else {
					errs = mgr.Discover(context.Background())
				}
				if len(errs) != 1 {
					t.Fatalf("errors = %v, want one", errs)
				}
				if got := errs[0].Error(); !strings.Contains(got, dir) || !strings.Contains(got, tc.want) {
					t.Fatalf("error = %q, want directory %q and %q", got, dir, tc.want)
				}
				if tc.name == "missing-runtime" {
					if !errors.Is(errs[0], exec.ErrNotFound) {
						t.Fatalf("error does not wrap exec.ErrNotFound: %v", errs[0])
					}
					if strings.Count(errs[0].Error(), dir) != 1 {
						t.Fatalf("directory should appear once: %v", errs[0])
					}
				}
			})
		}
	}
}

func TestDiscoverReportsMissingRuntimeWhenExtensionCannotStart(t *testing.T) {
	tmp := t.TempDir()
	extDir := filepath.Join(tmp, "extensions", "missing-runtime")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "extension.json"), []byte(`{"name":"missing-runtime","exec":"python3","language":"python"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Isolate PATH so this tests a missing language runtime rather than
	// relying on a made-up executable name that could theoretically exist.
	t.Setenv("PATH", filepath.Join(tmp, "empty-bin"))
	mgr := New(tmp, "", "0.0.0-test", "", "", nil)
	errs := mgr.Discover(context.Background())
	if len(errs) != 1 {
		t.Fatalf("discover errors = %v, want one", errs)
	}
	got := errs[0].Error()
	for _, want := range []string{
		`Extension missing-runtime failed to start.`,
		`exec: "python3"`,
		`declared language: "python"`,
		"executable file not found",
		extDir,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("error = %q, want it to contain %q", got, want)
		}
	}
	if strings.Count(got, extDir) != 1 {
		t.Fatalf("directory should appear once: %q", got)
	}
}
