package agent

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestZutfilePreOutputBound(t *testing.T) {
	for _, size := range []int{0, startupPreOutputLimit, startupPreOutputLimit + 1} {
		var out startupPreOutput
		input := strings.Repeat("x", size)
		n, err := out.Write([]byte(input))
		if err != nil || n != size {
			t.Fatalf("Write = %d, %v", n, err)
		}
		if out.Len() > startupPreOutputLimit {
			t.Fatalf("retained %d bytes", out.Len())
		}
		if strings.Contains(out.String(), "truncated") != (size > startupPreOutputLimit) {
			t.Fatalf("truncation for %d bytes", size)
		}
		n, err = out.Write([]byte("more"))
		if err != nil || n != 4 {
			t.Fatalf("continued Write = %d, %v", n, err)
		}
	}
}

func TestZutfilePreOutputTruncatesOnRuneBoundary(t *testing.T) {
	var out startupPreOutput
	// "é" is two bytes; an odd prefix makes the limit fall inside a rune.
	input := "x" + strings.Repeat("é", startupPreOutputLimit/2)
	if _, err := out.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(out.Bytes()) || out.Len() != startupPreOutputLimit-1 {
		t.Fatalf("retained %d bytes, valid=%v", out.Len(), utf8.Valid(out.Bytes()))
	}
}
