package extensions

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExtensionEnvironmentMinimizesAndExplicitlyCopiesHostValues(t *testing.T) {
	values := map[string]string{
		"PATH":         "/synthetic/bin",
		"HOME":         "/synthetic/home",
		"SECRET_TOKEN": "synthetic-token",
		"EMPTY_TOKEN":  "",
		"UNREQUESTED":  "must-not-leak",
	}
	lookup := func(name string) (string, bool) { value, ok := values[name]; return value, ok }
	env, err := extensionEnvironment([]string{"SECRET_TOKEN", "EMPTY_TOKEN"}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{"PATH=/synthetic/bin", "HOME=/synthetic/home", "SECRET_TOKEN=synthetic-token", "EMPTY_TOKEN="} {
		if !strings.Contains(joined, want) {
			t.Errorf("environment missing %q: %v", want, env)
		}
	}
	if strings.Contains(joined, "UNREQUESTED") {
		t.Fatalf("unrequested host value leaked: %v", env)
	}
}

func TestExtensionEnvironmentRejectsUnsafeRequests(t *testing.T) {
	lookup := func(string) (string, bool) { return "synthetic", true }
	for _, requested := range [][]string{
		{"lowercase"},
		{"BAD=NAME"},
		{"LD_PRELOAD"},
		{"DYLD_INSERT_LIBRARIES"},
		{"NODE_OPTIONS"},
		{"PATH"},
		{"TOKEN", "TOKEN"},
	} {
		if _, err := extensionEnvironment(requested, lookup); err == nil {
			t.Errorf("extensionEnvironment(%v) unexpectedly succeeded", requested)
		}
	}
}

func TestBoundedLogUsesRestrictivePermissionsAndDrainsOverflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extension.log")
	log, err := openBoundedLog(path)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), maxExtensionLogBytes+4096)
	if n, err := log.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(payload))
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != maxExtensionLogBytes {
		t.Fatalf("log size = %d, want %d", info.Size(), maxExtensionLogBytes)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("log permissions = %o, want 600", info.Mode().Perm())
	}
	if _, err := log.Write([]byte("after close")); err == nil {
		t.Fatal("write after close unexpectedly succeeded")
	}
}

func TestDiscoverPassesOnlyRequestedHostEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock extension uses /bin/sh")
	}
	t.Setenv("ZUT_SYNTHETIC_TOKEN", "allowed")
	t.Setenv("ZUT_UNREQUESTED_TOKEN", "hidden")
	root := t.TempDir()
	extDir := filepath.Join(root, "extensions", "envcheck")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
[ "$ZUT_SYNTHETIC_TOKEN" = allowed ] || exit 31
[ -z "${ZUT_UNREQUESTED_TOKEN+x}" ] || exit 32
printf '%s\n' '{"type":"hello","protocol_version":2,"name":"envcheck"}'
while IFS= read -r line; do
  case "$line" in *'"type":"shutdown"'*) printf '%s\n' '{"type":"shutdown_ack"}'; exit 0;; esac
done
`
	if err := os.WriteFile(filepath.Join(extDir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"envcheck","exec":"./run.sh","host_env":["ZUT_SYNTHETIC_TOKEN"]}`
	if err := os.WriteFile(filepath.Join(extDir, "extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := New(root, "", "0.0.0-test", "", "", nil)
	if errs := mgr.Discover(context.Background()); len(errs) != 0 {
		t.Fatalf("Discover errors: %v", errs)
	}
	mgr.Stop(time.Second)
}

func TestManifestNameCannotEscapeLogDirectory(t *testing.T) {
	root := t.TempDir()
	extDir := filepath.Join(root, "extensions", "bad")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "extension.json"), []byte(`{"name":"../outside","exec":"missing"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := New(root, "", "0.0.0-test", "", "", nil)
	errs := mgr.Discover(context.Background())
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "manifest: name") {
		t.Fatalf("Discover errors = %v, want invalid name", errs)
	}
}
