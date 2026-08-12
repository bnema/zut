package modes

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/tui"
)

type cleanupTestTerminal struct {
	bytes.Buffer
}

func (t *cleanupTestTerminal) Size() (int, int)        { return 80, 24 }
func (t *cleanupTestTerminal) OnResize(func())         {}
func (t *cleanupTestTerminal) SetNonblock(bool) error  { return nil }
func (t *cleanupTestTerminal) ReadByte() (byte, error) { return 0, io.EOF }
func (t *cleanupTestTerminal) EnterRaw() (func() error, error) {
	return func() error { return nil }, nil
}
func (t *cleanupTestTerminal) PeekByteTimeout(time.Duration) (byte, bool, error) {
	return 0, false, io.EOF
}

func TestInteractiveRunClearsVisibleFrameOnExit(t *testing.T) {
	term := &cleanupTestTerminal{}
	interactive := NewInteractive(InteractiveConfig{Terminal: term})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := interactive.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}

	cleanup := tui.SeqResetScrollRegion + tui.SeqDeleteKittyImages +
		tui.SeqEnhancedKeyboardOff + tui.SeqBracketedPasteOff +
		tui.ResetCursorColor() + tui.ResetCursorShape() +
		tui.SeqClearScreenNoHome + tui.MoveTo(1, 1) + tui.SeqShowCursor
	if got := term.String(); !strings.HasSuffix(got, cleanup) {
		t.Fatalf("terminal output does not end with frame cleanup\ngot suffix: %q\nwant:       %q", got, cleanup)
	}
}
