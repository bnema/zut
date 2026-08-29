package tui

import (
	"strconv"
	"strings"
	"time"
)

// Key is a parsed keypress.
type Key struct {
	Kind  KeyKind
	Rune  rune   // for KeyRune
	Paste string // for KeyPaste
	Ctrl  bool
	Alt   bool
	Shift bool
	Super bool
}

type KeyKind int

const (
	KeyRune KeyKind = iota
	KeyEnter
	KeyBackspace
	KeyTab
	KeyShiftTab
	KeyEsc
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPageUp
	KeyPageDown
	KeyDelete
	KeyCtrlC
	KeyCtrlD
	KeyCtrlL
	KeyCtrlU
	KeyCtrlK
	KeyCtrlA
	KeyCtrlE
	KeyCtrlW
	KeyCtrlO
	KeyPaste
	KeyPasteClipboard
	KeyMouseWheelUp
	KeyMouseWheelDown
	KeyUnknown
)

// Reader parses a byte stream into Key events. It understands basic
// xterm escape sequences and bracketed paste.
type Reader struct {
	src  func() (byte, error)
	peek func(time.Duration) (byte, bool, error) // optional; may be nil

	// pending contains bytes consumed while deciding whether an Esc was
	// bare. In particular, a second Esc belongs to the next Read, not to
	// the Alt+<char> case handled by readEscape.
	pending []byte

	// A Reader without a peek callback still needs to preserve the old
	// NewReader API and parse Alt/CSI input. The pump lets its bounded
	// lookahead wait without calling the blocking source on the Read caller's
	// goroutine. There is never more than one outstanding source call: after a
	// timeout that call is reused by the next Read rather than replaced.
	//
	// The source callback has no cancellation contract, so an implementation
	// that blocks forever can keep this one goroutine alive forever. That
	// limitation is intentional; starting another goroutine for each isolated
	// Esc would leak an unbounded number of blocked goroutines and could also
	// reorder bytes from an uncancellable source.
	pump    *readerPump
	pumpErr error
}

type readerResult struct {
	b   byte
	err error
}

type readerPump struct {
	result chan readerResult
}

func newReaderPump(read func() (byte, error)) *readerPump {
	p := &readerPump{result: make(chan readerResult, 1)}
	go func() {
		b, err := read()
		p.result <- readerResult{b: b, err: err}
	}()
	return p
}

// NewReader returns a Reader that pulls bytes from read.
func NewReader(read func() (byte, error)) *Reader { return &Reader{src: read} }

// NewReaderWithPeek returns a Reader that pulls bytes from read and uses
// peek to disambiguate bare Esc from the start of an escape sequence.
func NewReaderWithPeek(read func() (byte, error), peek func(time.Duration) (byte, bool, error)) *Reader {
	return &Reader{src: read, peek: peek}
}

// readByte returns the next source byte, including any byte held back after
// an escape lookahead.
func (r *Reader) readByte() (byte, error) {
	if len(r.pending) != 0 {
		b := r.pending[0]
		r.pending = r.pending[1:]
		return b, nil
	}
	if r.pumpErr != nil {
		return 0, r.pumpErr
	}
	if r.pump == nil {
		return r.src()
	}
	result := <-r.pump.result
	r.pump = nil
	if result.err != nil {
		r.pumpErr = result.err
	}
	return result.b, result.err
}

// readByteTimeout returns one source byte without waiting longer than d. A
// source call left outstanding after a timeout is consumed by the next Read.
func (r *Reader) readByteTimeout(d time.Duration) (byte, bool, error) {
	if r.pumpErr != nil {
		return 0, false, r.pumpErr
	}
	if r.pump == nil {
		r.pump = newReaderPump(r.src)
	}

	var result readerResult
	if d <= 0 {
		select {
		case result = <-r.pump.result:
		default:
			return 0, false, nil
		}
	} else {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case result = <-r.pump.result:
		case <-timer.C:
			return 0, false, nil
		}
	}
	r.pump = nil
	if result.err != nil {
		r.pumpErr = result.err
		return 0, false, result.err
	}
	return result.b, true, nil
}

// Read returns the next parsed Key.
func (r *Reader) Read() (Key, error) {
	b, err := r.readByte()
	if err != nil {
		return Key{}, err
	}
	switch {
	case b == 0x03:
		return Key{Kind: KeyCtrlC}, nil
	case b == 0x04:
		return Key{Kind: KeyCtrlD}, nil
	case b == 0x0c:
		return Key{Kind: KeyCtrlL}, nil
	case b == 0x15:
		return Key{Kind: KeyCtrlU}, nil
	case b == 0x0b:
		return Key{Kind: KeyCtrlK}, nil
	case b == 0x01:
		return Key{Kind: KeyCtrlA}, nil
	case b == 0x05:
		return Key{Kind: KeyCtrlE}, nil
	case b == 0x17:
		return Key{Kind: KeyCtrlW}, nil
	case b == 0x0f:
		return Key{Kind: KeyCtrlO}, nil
	case b == 0x16:
		return Key{Kind: KeyPasteClipboard, Ctrl: true}, nil
	case b == '\r', b == '\n':
		return Key{Kind: KeyEnter}, nil
	case b == '\t':
		return Key{Kind: KeyTab}, nil
	case b == 0x7f, b == 0x08:
		return Key{Kind: KeyBackspace}, nil
	case b == 0x1b:
		return r.readEscape()
	case b < 0x20:
		return Key{Kind: KeyUnknown}, nil
	}
	// UTF-8 multibyte?
	if b < 0x80 {
		return Key{Kind: KeyRune, Rune: rune(b)}, nil
	}
	// Decode UTF-8 (up to 4 bytes).
	n := utf8Len(b)
	buf := []byte{b}
	for i := 1; i < n; i++ {
		bb, err := r.readByte()
		if err != nil {
			return Key{}, err
		}
		buf = append(buf, bb)
	}
	rn, _ := decodeRune(buf)
	return Key{Kind: KeyRune, Rune: rn}, nil
}

func utf8Len(b byte) int {
	switch {
	case b&0xe0 == 0xc0:
		return 2
	case b&0xf0 == 0xe0:
		return 3
	case b&0xf8 == 0xf0:
		return 4
	}
	return 1
}

func decodeRune(b []byte) (rune, int) {
	// Minimal decoder; invalid runes become U+FFFD.
	if len(b) == 1 {
		return rune(b[0]), 1
	}
	var r rune
	switch len(b) {
	case 2:
		r = rune(b[0]&0x1f)<<6 | rune(b[1]&0x3f)
	case 3:
		r = rune(b[0]&0x0f)<<12 | rune(b[1]&0x3f)<<6 | rune(b[2]&0x3f)
	case 4:
		r = rune(b[0]&0x07)<<18 | rune(b[1]&0x3f)<<12 | rune(b[2]&0x3f)<<6 | rune(b[3]&0x3f)
	default:
		r = 0xFFFD
	}
	return r, len(b)
}

// readEscape handles sequences starting with 0x1b.
func (r *Reader) readEscape() (Key, error) {
	// Bare ESC: maybe followed by another byte within a short window.
	b, have, err := r.readEscapeNext(50 * time.Millisecond)
	if err != nil || !have {
		return Key{Kind: KeyEsc}, nil
	}
	if b == 0x1b {
		// A second raw ESC is a separate key, not Alt+ESC. Keep it for the
		// next Read so Esc Esc produces two bare KeyEsc events. This is the
		// one lookahead case that is not intentionally treated as Alt input.
		r.pending = append(r.pending, b)
		return Key{Kind: KeyEsc}, nil
	}
	switch b {
	case '[':
		return r.readCSI()
	case 'O':
		// SS3 sequences (function keys in some terminals).
		c, err := r.readByte()
		if err != nil {
			return Key{}, err
		}
		switch c {
		case 'H':
			return Key{Kind: KeyHome}, nil
		case 'F':
			return Key{Kind: KeyEnd}, nil
		}
		return Key{Kind: KeyUnknown}, nil
	case 0x7f, 0x08:
		// Alt+Backspace (Option+Delete on macOS) — most terminals send
		// ESC + DEL for this. Surface as a dedicated "alt backspace"
		// so the editor can map it to delete-word.
		return Key{Kind: KeyBackspace, Alt: true}, nil
	case 'b':
		// Emacs-style word-left, also emitted by some terminals for
		// Option+LeftArrow.
		return Key{Kind: KeyLeft, Alt: true}, nil
	case 'f':
		// Emacs-style word-right, also emitted for Option+RightArrow.
		return Key{Kind: KeyRight, Alt: true}, nil
	default:
		// Alt+<char>
		if b < 0x80 {
			return Key{Kind: KeyRune, Rune: rune(b), Alt: true}, nil
		}
	}
	return Key{Kind: KeyUnknown}, nil
}

// readEscapeNext tries to read one byte within d. If peek is available
// we use it. NewReader's source pump supplies the same bounded behavior
// when no peek callback is provided, so a bare Esc never waits forever for
// a second byte. A follow-on printable byte, DEL, b, or f remains
// intentionally ambiguous with Alt input and keeps the historical mapping
// in readEscape.
func (r *Reader) readEscapeNext(d time.Duration) (byte, bool, error) {
	if r.peek != nil {
		return r.peek(d)
	}
	return r.readByteTimeout(d)
}

// readCSI parses a CSI sequence after ESC [.
func (r *Reader) readCSI() (Key, error) {
	var params []byte
	for {
		c, err := r.readByte()
		if err != nil {
			return Key{}, err
		}
		if c >= 0x30 && c <= 0x3f {
			params = append(params, c)
			continue
		}
		// Final byte.
		return r.dispatchCSI(string(params), c), nil
	}
}

func (r *Reader) dispatchCSI(params string, final byte) Key {
	// SGR mouse mode: CSI < button ; x ; y M/m. Wheel events use
	// button codes 64 (up) and 65 (down). We ignore coordinates for
	// now; the chat view only needs scroll direction.
	if strings.HasPrefix(params, "<") && (final == 'M' || final == 'm') {
		parts := strings.Split(strings.TrimPrefix(params, "<"), ";")
		if len(parts) >= 1 {
			switch parts[0] {
			case "64":
				return Key{Kind: KeyMouseWheelUp}
			case "65":
				return Key{Kind: KeyMouseWheelDown}
			}
		}
		return Key{Kind: KeyUnknown}
	}

	shift, alt, super := parseCSIModifiers(params)
	if final == 'u' {
		if key, ok := parseCSIU(params); ok {
			return key
		}
	}
	if final == '~' {
		if key, ok := parseModifyOtherKeys(params); ok {
			return key
		}
	}
	switch final {
	case 'A':
		return Key{Kind: KeyUp, Alt: alt, Shift: shift, Super: super}
	case 'B':
		return Key{Kind: KeyDown, Alt: alt, Shift: shift, Super: super}
	case 'C':
		return Key{Kind: KeyRight, Alt: alt, Shift: shift, Super: super}
	case 'D':
		return Key{Kind: KeyLeft, Alt: alt, Shift: shift, Super: super}
	case 'H':
		return Key{Kind: KeyHome}
	case 'F':
		return Key{Kind: KeyEnd}
	case 'Z':
		return Key{Kind: KeyShiftTab}
	case '~':
		switch params {
		case "3":
			return Key{Kind: KeyDelete}
		case "5":
			return Key{Kind: KeyPageUp}
		case "6":
			return Key{Kind: KeyPageDown}
		case "200":
			// Start of bracketed paste.
			return r.readPaste()
		}
	}
	return Key{Kind: KeyUnknown}
}

func parseCSIModifiers(params string) (shift, alt, super bool) {
	if params == "" {
		return false, false, false
	}
	i := strings.LastIndexByte(params, ';')
	if i < 0 || i+1 >= len(params) {
		return false, false, false
	}
	mod, ok := parseModifierParam(params[i+1:])
	if !ok {
		return false, false, false
	}
	shift, alt, _, super = modifierBits(mod)
	return shift, alt, super
}

func parseCSIU(params string) (Key, bool) {
	parts := strings.Split(params, ";")
	if len(parts) == 0 {
		return Key{}, false
	}
	code, err := strconv.Atoi(parts[0])
	if err != nil {
		return Key{}, false
	}
	mod := 1
	if len(parts) >= 2 {
		var ok bool
		mod, ok = parseModifierParam(parts[1])
		if !ok {
			return Key{}, false
		}
	}
	return keyFromModifiedCode(code, mod)
}

func parseModifyOtherKeys(params string) (Key, bool) {
	parts := strings.Split(params, ";")
	if len(parts) != 3 || parts[0] != "27" {
		return Key{}, false
	}
	mod, ok := parseModifierParam(parts[1])
	if !ok {
		return Key{}, false
	}
	code, err := strconv.Atoi(parts[2])
	if err != nil {
		return Key{}, false
	}
	return keyFromModifiedCode(code, mod)
}

func parseModifierParam(s string) (int, bool) {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	mod, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return mod, true
}

func modifierBits(mod int) (shift, alt, ctrl, super bool) {
	bits := mod - 1
	return bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0 || bits&32 != 0
}

func keyFromModifiedCode(code, mod int) (Key, bool) {
	shift, alt, ctrl, super := modifierBits(mod)
	// Kitty keyboard protocol (CSI ... u) reports control keys as their
	// codepoints: Esc=27, Enter=13, Tab=9, Backspace=127. Without the
	// enhanced-mode handling these arrive as raw bytes; with it enabled
	// they come through here, so map them back to their dedicated keys.
	switch code {
	case 13:
		return Key{Kind: KeyEnter, Shift: shift, Alt: alt, Ctrl: ctrl, Super: super}, true
	case 27:
		return Key{Kind: KeyEsc, Shift: shift, Alt: alt, Ctrl: ctrl, Super: super}, true
	case 9:
		if shift {
			return Key{Kind: KeyShiftTab, Alt: alt, Ctrl: ctrl, Super: super}, true
		}
		return Key{Kind: KeyTab, Shift: shift, Alt: alt, Ctrl: ctrl, Super: super}, true
	case 127, 8:
		return Key{Kind: KeyBackspace, Shift: shift, Alt: alt, Ctrl: ctrl, Super: super}, true
	}
	if ctrl {
		switch code {
		case 'c', 'C':
			return Key{Kind: KeyCtrlC, Shift: shift, Alt: alt, Ctrl: true}, true
		case 'd', 'D':
			return Key{Kind: KeyCtrlD, Shift: shift, Alt: alt, Ctrl: true}, true
		case 'l', 'L':
			return Key{Kind: KeyCtrlL, Shift: shift, Alt: alt, Ctrl: true}, true
		case 'u', 'U':
			return Key{Kind: KeyCtrlU, Shift: shift, Alt: alt, Ctrl: true}, true
		case 'k', 'K':
			return Key{Kind: KeyCtrlK, Shift: shift, Alt: alt, Ctrl: true}, true
		case 'a', 'A':
			return Key{Kind: KeyCtrlA, Shift: shift, Alt: alt, Ctrl: true}, true
		case 'e', 'E':
			return Key{Kind: KeyCtrlE, Shift: shift, Alt: alt, Ctrl: true}, true
		case 'w', 'W':
			return Key{Kind: KeyCtrlW, Shift: shift, Alt: alt, Ctrl: true}, true
		case 'o', 'O':
			return Key{Kind: KeyCtrlO, Shift: shift, Alt: alt, Ctrl: true}, true
		case 'v', 'V':
			return Key{Kind: KeyPasteClipboard, Shift: shift, Alt: alt, Ctrl: true}, true
		}
	}
	if code >= '0' && code <= '9' {
		return Key{Kind: KeyRune, Rune: rune(code), Shift: shift, Alt: alt, Ctrl: ctrl, Super: super}, true
	}
	return Key{}, false
}

// readPaste reads until ESC [ 2 0 1 ~ and returns the pasted text.
func (r *Reader) readPaste() Key {
	var sb strings.Builder
	const end = "\x1b[201~"
	tail := make([]byte, 0, len(end))
	for {
		b, err := r.readByte()
		if err != nil {
			break
		}
		tail = append(tail, b)
		if len(tail) > len(end) {
			sb.WriteByte(tail[0])
			tail = tail[1:]
		}
		if string(tail) == end {
			break
		}
	}
	return Key{Kind: KeyPaste, Paste: sb.String()}
}
