package agent

import (
	"context"
	"errors"
	"os"
	"testing"
)

// An orchestrator waiting for a piped prompt must stop when its signal
// context ends instead of blocking on an open pipe.
func TestReadAllStdinContextReturnsOnCancel(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(); _ = r.Close() })
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readAllStdinContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
