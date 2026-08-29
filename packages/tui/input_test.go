package tui

import (
	"io"
	"testing"
	"time"
)

func TestReaderParsesCSIUShiftEnter(t *testing.T) {
	k := readKey(t, "\x1b[13;2u")
	if k.Kind != KeyEnter || !k.Shift || k.Alt {
		t.Fatalf("Read kind=%v shift=%v alt=%v, want shift+enter", k.Kind, k.Shift, k.Alt)
	}
}

func TestReaderParsesModifyOtherKeysShiftEnter(t *testing.T) {
	k := readKey(t, "\x1b[27;2;13~")
	if k.Kind != KeyEnter || !k.Shift || k.Alt {
		t.Fatalf("Read kind=%v shift=%v alt=%v, want shift+enter", k.Kind, k.Shift, k.Alt)
	}
}

func TestReaderParsesCSIUCtrlC(t *testing.T) {
	k := readKey(t, "\x1b[99;5u")
	if k.Kind != KeyCtrlC || !k.Ctrl {
		t.Fatalf("Read kind=%v ctrl=%v, want ctrl+c", k.Kind, k.Ctrl)
	}
}

func TestReaderParsesCSIUCtrlNumber(t *testing.T) {
	k := readKey(t, "\x1b[49;5u")
	if k.Kind != KeyRune || k.Rune != '1' || !k.Ctrl {
		t.Fatalf("Read kind=%v rune=%q ctrl=%v, want ctrl+1", k.Kind, k.Rune, k.Ctrl)
	}
}

func TestReaderParsesCSIUSuperNumber(t *testing.T) {
	k := readKey(t, "\x1b[50;9u")
	if k.Kind != KeyRune || k.Rune != '2' || !k.Super {
		t.Fatalf("Read kind=%v rune=%q super=%v, want super+2", k.Kind, k.Rune, k.Super)
	}
}

func TestReaderParsesCSIUSuperNumberWithEventType(t *testing.T) {
	k := readKey(t, "\x1b[51;9:3u")
	if k.Kind != KeyRune || k.Rune != '3' || !k.Super {
		t.Fatalf("Read kind=%v rune=%q super=%v, want super+3", k.Kind, k.Rune, k.Super)
	}
}

func TestReaderParsesCSIUHyperNumberAsSuper(t *testing.T) {
	k := readKey(t, "\x1b[52;33u")
	if k.Kind != KeyRune || k.Rune != '4' || !k.Super {
		t.Fatalf("Read kind=%v rune=%q super=%v, want hyper+4 as super", k.Kind, k.Rune, k.Super)
	}
}

func TestReaderParsesRawCtrlVAsClipboardPaste(t *testing.T) {
	k := readKey(t, "\x16")
	if k.Kind != KeyPasteClipboard || !k.Ctrl {
		t.Fatalf("Read kind=%v ctrl=%v, want ctrl+v clipboard paste", k.Kind, k.Ctrl)
	}
}

func TestReaderParsesCSIUCtrlVAsClipboardPaste(t *testing.T) {
	k := readKey(t, "\x1b[118;5u")
	if k.Kind != KeyPasteClipboard || !k.Ctrl {
		t.Fatalf("Read kind=%v ctrl=%v, want enhanced ctrl+v clipboard paste", k.Kind, k.Ctrl)
	}
}

func TestReaderParsesModifyOtherKeysCtrlC(t *testing.T) {
	k := readKey(t, "\x1b[27;5;99~")
	if k.Kind != KeyCtrlC || !k.Ctrl {
		t.Fatalf("Read kind=%v ctrl=%v, want ctrl+c", k.Kind, k.Ctrl)
	}
}

func TestReaderParsesCSIUEsc(t *testing.T) {
	k := readKey(t, "\x1b[27u")
	if k.Kind != KeyEsc {
		t.Fatalf("Read kind=%v, want esc", k.Kind)
	}
}

func TestReaderParsesCSIUTabAndBackspace(t *testing.T) {
	if k := readKey(t, "\x1b[9u"); k.Kind != KeyTab {
		t.Fatalf("Read kind=%v, want tab", k.Kind)
	}
	if k := readKey(t, "\x1b[9;2u"); k.Kind != KeyShiftTab {
		t.Fatalf("Read kind=%v, want shift-tab", k.Kind)
	}
	if k := readKey(t, "\x1b[127u"); k.Kind != KeyBackspace {
		t.Fatalf("Read kind=%v, want backspace", k.Kind)
	}
}

func TestReaderParsesRawEscapeEscapeAsTwoEscapes(t *testing.T) {
	r := newPeekReader("\x1b\x1b")
	if k := readReaderKey(t, r); k.Kind != KeyEsc {
		t.Fatalf("first Read kind=%v, want esc", k.Kind)
	}
	if k := readReaderKey(t, r); k.Kind != KeyEsc {
		t.Fatalf("second Read kind=%v, want esc", k.Kind)
	}
}

func TestReaderParsesRawEscapeEscapeCSIAsEscapeThenUp(t *testing.T) {
	r := newPeekReader("\x1b\x1b[A")
	if k := readReaderKey(t, r); k.Kind != KeyEsc {
		t.Fatalf("first Read kind=%v, want esc", k.Kind)
	}
	if k := readReaderKey(t, r); k.Kind != KeyUp {
		t.Fatalf("second Read kind=%v, want up", k.Kind)
	}
}

func TestReaderPreservesDoubleEscapePrintableAmbiguity(t *testing.T) {
	r := newPeekReader("\x1b\x1bx")
	if got := readReaderKey(t, r); got.Kind != KeyEsc {
		t.Fatalf("first Read = %+v, want bare escape", got)
	}
	if got := readReaderKey(t, r); got.Kind != KeyRune || got.Rune != 'x' || !got.Alt {
		t.Fatalf("second Read = %+v, want Alt+x after the documented ambiguity", got)
	}
}

func TestReaderPreservesAmbiguousAltEscapeMappings(t *testing.T) {
	cases := []struct {
		name string
		seq  string
		want Key
	}{
		{name: "printable", seq: "\x1bx", want: Key{Kind: KeyRune, Rune: 'x', Alt: true}},
		{name: "delete", seq: "\x1b\x7f", want: Key{Kind: KeyBackspace, Alt: true}},
		{name: "word-left", seq: "\x1bb", want: Key{Kind: KeyLeft, Alt: true}},
		{name: "word-right", seq: "\x1bf", want: Key{Kind: KeyRight, Alt: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := readReaderKey(t, newPeekReader(tc.seq))
			if got != tc.want {
				t.Fatalf("Read(%q) = %+v, want %+v", tc.seq, got, tc.want)
			}
		})
	}
}

func TestReaderWithoutPeekDoesNotBlockBareEscapeOrStartAnotherPump(t *testing.T) {
	release := make(chan struct{})
	lookaheadStarted := make(chan struct{})
	calls := 0
	r := NewReader(func() (byte, error) {
		calls++
		if calls == 1 {
			return 0x1b, nil
		}
		if calls == 2 {
			close(lookaheadStarted)
			<-release
			return 'x', nil
		}
		return 0, io.EOF
	})

	readDone := make(chan struct{})
	var got Key
	var err error
	go func() {
		got, err = r.Read()
		close(readDone)
	}()
	select {
	case <-lookaheadStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("lookahead source was not started")
	}
	select {
	case <-readDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Read blocked while disambiguating bare escape")
	}
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Kind != KeyEsc {
		t.Fatalf("Read kind=%v, want esc", got.Kind)
	}

	// The timed-out source call is still the sole owner of the next byte.
	// Releasing it must make that byte available to the next Read without
	// starting a second blocked callback.
	close(release)
	got, err = r.Read()
	if err != nil {
		t.Fatalf("Read after lookahead: %v", err)
	}
	if got.Kind != KeyRune || got.Rune != 'x' || calls != 2 {
		t.Fatalf("Read after lookahead = %+v with %d source calls, want x with 2 calls", got, calls)
	}
}

func TestReaderParsesSGRMouseWheel(t *testing.T) {
	cases := []struct {
		seq  string
		want KeyKind
	}{
		{"\x1b[<64;10;20M", KeyMouseWheelUp},
		{"\x1b[<65;10;20M", KeyMouseWheelDown},
	}
	for _, tc := range cases {
		k := readKey(t, tc.seq)
		if k.Kind != tc.want {
			t.Fatalf("Read(%q) kind=%v, want %v", tc.seq, k.Kind, tc.want)
		}
	}
}

func readKey(t *testing.T, seq string) Key {
	t.Helper()
	return readReaderKey(t, NewReader(func() (byte, error) {
		if len(seq) == 0 {
			return 0, io.EOF
		}
		b := seq[0]
		seq = seq[1:]
		return b, nil
	}))
}

func newPeekReader(seq string) *Reader {
	idx := 0
	read := func() (byte, error) {
		if idx >= len(seq) {
			return 0, io.EOF
		}
		b := seq[idx]
		idx++
		return b, nil
	}
	peek := func(time.Duration) (byte, bool, error) {
		if idx >= len(seq) {
			return 0, false, nil
		}
		b := seq[idx]
		idx++
		return b, true, nil
	}
	return NewReaderWithPeek(read, peek)
}

func readReaderKey(t *testing.T, r *Reader) Key {
	t.Helper()
	k, err := r.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return k
}
