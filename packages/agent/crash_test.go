package agent

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCrashOutput(t *testing.T) {
	if home := os.Getenv("ZUT_TEST_CRASH_HOME"); home != "" {
		setupCrashOutput(home, os.Getenv("ZUT_TEST_CRASH_PROCESS"), nil)
		panic("zut crash output regression")
	}
	for _, process := range []string{"client", "daemon"} {
		t.Run(process, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "logs", process+"-crash.log")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			const prefix = "previous crash\n"
			if err := os.WriteFile(path, []byte(prefix), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashOutput$")
			cmd.Env = append(os.Environ(), "ZUT_TEST_CRASH_HOME="+home, "ZUT_TEST_CRASH_PROCESS="+process)
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("expected panic: %s", out)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(data), prefix) || !strings.Contains(string(data), "panic: zut crash output regression") {
				t.Fatalf("crash output missing or overwritten: %s", data)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && fi.Mode().Perm() != 0600 {
				t.Fatalf("permissions: %v", fi.Mode())
			}
		})
	}
}

func TestCrashOutputOpenFailureIsBestEffort(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "logs"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	setupCrashOutput(home, "client", slog.New(slog.NewTextHandler(&buf, nil)))
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("missing warning: %s", buf.String())
	}
	setupCrashOutput(home, "client", nil)
}
