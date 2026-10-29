package agent

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestRunZutfileStartupPreShellFailure(t *testing.T) {
	pre := "!false"
	if runtime.GOOS == "windows" {
		pre = "!exit 1"
	}
	err := runZutfileStartupPreEnv(context.Background(), pre, t.TempDir(), os.Environ(), nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected entry.pre shell failure")
	}
	if !strings.Contains(err.Error(), "entry.pre") {
		t.Fatalf("error = %v, want entry.pre context", err)
	}
}

func TestRunZutfileStartupPreEmpty(t *testing.T) {
	if err := runZutfileStartupPreEnv(context.Background(), "  ", t.TempDir(), os.Environ(), nil, nil, nil, nil); err != nil {
		t.Fatalf("empty pre: %v", err)
	}
}

func TestRunZutfileStartupPreShellStreams(t *testing.T) {
	cmd := "!printf streamed-pre"
	if runtime.GOOS == "windows" {
		cmd = "!echo streamed-pre"
	}
	var out strings.Builder
	if err := runZutfileStartupPreEnv(context.Background(), cmd, t.TempDir(), os.Environ(), nil, nil, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "streamed-pre") {
		t.Fatalf("shell output not streamed: %q", out.String())
	}
}
