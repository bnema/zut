package tui

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// SchemeEvent is a reported terminal color-scheme preference.
const (
	SeqQueryColorScheme = "\x1b[?996n"
	SeqQueryMode2031    = "\x1b[?2031$p"
	SeqEnableMode2031   = "\x1b[?2031h"
	SeqDisableMode2031  = "\x1b[?2031l"
)

// AppearanceQuery returns the complete profile query used at startup and on a
// replacement generation. The ANSI palette is requested in one OSC 4 batch.
func AppearanceQuery() string {
	var b strings.Builder
	b.WriteString(SeqQueryColorScheme)
	b.WriteString("\x1b]10;?\a\x1b]11;?\a\x1b]4;")
	for slot := 0; slot < 16; slot++ {
		if slot > 0 {
			b.WriteByte(';')
		}
		b.WriteString(strconv.Itoa(slot))
		b.WriteString(";?")
	}
	b.WriteByte('\a')
	return b.String()
}

// SchemeEvent is a reported terminal color-scheme preference.
type SchemeEvent struct{ Light bool }

// ColorEvent is one default foreground/background or ANSI-palette reply.
type ColorEvent struct {
	Kind  int // 10 foreground, 11 background, 4 palette
	Slot  int
	Color TerminalColor
}

// ModeReportEvent is a DECRQM response. Status follows the terminal protocol:
// 1/3 set, 2 reset, 0/4 unsupported or permanently reset.
type ModeReportEvent struct {
	Mode   int
	Status int
}

// InputEvent is emitted by the appearance-aware input path. Exactly one field
// is set. Key bytes that do not form an accepted protocol reply remain keys.
type InputEvent struct {
	Key    *Key
	Scheme *SchemeEvent
	Color  *ColorEvent
	Mode   *ModeReportEvent
}

// AppearanceParser controls which terminal replies may be consumed. Keeping
// the gate here prevents OSC-looking editor input from disappearing unless zut
// actively issued a matching query, while 997 notifications are accepted only
// after notification support is known.
type AppearanceParser struct {
	mu            sync.RWMutex
	pendingColors bool
	notifications bool
}

func (p *AppearanceParser) SetPendingColors(pending bool) {
	p.mu.Lock()
	p.pendingColors = pending
	p.mu.Unlock()
}

func (p *AppearanceParser) SetNotifications(enabled bool) {
	p.mu.Lock()
	p.notifications = enabled
	p.mu.Unlock()
}

func (p *AppearanceParser) acceptsColors() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pendingColors
}

func (p *AppearanceParser) acceptsNotifications() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.notifications
}

// ParseAppearanceOSC parses one complete BEL/ST-terminated OSC sequence.
// Combined OSC 4 responses produce one ColorEvent for every valid pair.
func ParseAppearanceOSC(raw []byte) []ColorEvent {
	s := strings.TrimSuffix(string(raw), "\a")
	s = strings.TrimSuffix(s, "\x1b\\")
	if !strings.HasPrefix(s, "\x1b]") {
		return nil
	}
	body := strings.TrimPrefix(s, "\x1b]")
	parts := strings.Split(body, ";")
	if len(parts) < 2 {
		return nil
	}
	kind, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil
	}
	switch kind {
	case 10, 11:
		color, ok := parseTerminalRGB(strings.Join(parts[1:], ";"))
		if !ok {
			return nil
		}
		return []ColorEvent{{Kind: kind, Slot: -1, Color: color}}
	case 4:
		var events []ColorEvent
		for idx := 1; idx+1 < len(parts); idx += 2 {
			slot, err := strconv.Atoi(parts[idx])
			if err != nil || slot < 0 || slot >= 16 {
				continue
			}
			color, ok := parseTerminalRGB(parts[idx+1])
			if ok {
				events = append(events, ColorEvent{Kind: 4, Slot: slot, Color: color})
			}
		}
		return events
	default:
		return nil
	}
}

// ParseAppearanceCSI parses the two runtime CSI response families used by
// terminal-owned themes: current scheme (997) and mode ownership (DECRQM).
func ParseAppearanceCSI(raw []byte) (scheme *SchemeEvent, mode *ModeReportEvent) {
	s := string(raw)
	if !strings.HasPrefix(s, "\x1b[") {
		return nil, nil
	}
	body := strings.TrimPrefix(s, "\x1b[")
	if strings.HasPrefix(body, "?997;") && strings.HasSuffix(body, "n") {
		value, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(body, "?997;"), "n"))
		if err == nil && (value == 1 || value == 2) {
			return &SchemeEvent{Light: value == 2}, nil
		}
	}
	if strings.HasPrefix(body, "?2031;") && strings.HasSuffix(body, "$y") {
		value, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(body, "?2031;"), "$y"))
		if err == nil {
			return nil, &ModeReportEvent{Mode: 2031, Status: value}
		}
	}
	return nil, nil
}

// AppearanceSource filters accepted terminal appearance replies before they
// reach Reader. It deliberately leaves bracketed-paste payload opaque.
type AppearanceSource struct {
	read   func() (byte, error)
	peek   func(time.Duration) (byte, bool, error)
	parser *AppearanceParser
	emit   func(InputEvent)

	pending   []byte
	inPaste   bool
	pasteTail []byte
}

func NewAppearanceSource(read func() (byte, error), peek func(time.Duration) (byte, bool, error), parser *AppearanceParser, emit func(InputEvent)) *AppearanceSource {
	return &AppearanceSource{read: read, peek: peek, parser: parser, emit: emit}
}

func (s *AppearanceSource) ReadByte() (byte, error) {
	if len(s.pending) > 0 {
		b := s.pending[0]
		s.pending = s.pending[1:]
		return b, nil
	}
	for {
		b, err := s.read()
		if err != nil || b != '\x1b' || s.inPaste || s.peek == nil {
			s.trackPaste(b)
			return b, err
		}
		next, ok, err := s.peek(time.Millisecond)
		if err != nil || !ok || (next != ']' && next != '[') {
			if ok {
				s.pending = append(s.pending, next)
			}
			return b, err
		}
		raw := []byte{'\x1b', next}
		if next == ']' {
			sequence, err := s.readOSC(raw)
			if err != nil {
				return 0, err
			}
			if events := ParseAppearanceOSC(sequence); len(events) != 0 && s.parser != nil && s.parser.acceptsColors() {
				for _, event := range events {
					e := event
					s.emit(InputEvent{Color: &e})
				}
				continue
			}
			s.pending = append(s.pending, sequence[1:]...)
			return '\x1b', nil
		}
		sequence, err := s.readCSI(raw)
		if err != nil {
			return 0, err
		}
		if bytesEqual(sequence, []byte("\x1b[200~")) {
			s.inPaste = true
		}
		scheme, mode := ParseAppearanceCSI(sequence)
		if scheme != nil && s.parser != nil && (s.parser.acceptsColors() || s.parser.acceptsNotifications()) {
			s.emit(InputEvent{Scheme: scheme})
			continue
		}
		if mode != nil {
			s.emit(InputEvent{Mode: mode})
			continue
		}
		s.pending = append(s.pending, sequence[1:]...)
		return '\x1b', nil
	}
}

func (s *AppearanceSource) readOSC(raw []byte) ([]byte, error) {
	const limit = 4096
	for len(raw) < limit {
		b, err := s.read()
		if err != nil {
			return raw, err
		}
		raw = append(raw, b)
		if b == '\a' || (len(raw) >= 2 && raw[len(raw)-2] == '\x1b' && b == '\\') {
			return raw, nil
		}
	}
	return raw, nil
}

func (s *AppearanceSource) readCSI(raw []byte) ([]byte, error) {
	const limit = 256
	for len(raw) < limit {
		b, err := s.read()
		if err != nil {
			return raw, err
		}
		raw = append(raw, b)
		if b >= 0x40 && b <= 0x7e {
			return raw, nil
		}
	}
	return raw, nil
}

func (s *AppearanceSource) trackPaste(b byte) {
	if !s.inPaste {
		return
	}
	const end = "\x1b[201~"
	s.pasteTail = append(s.pasteTail, b)
	if len(s.pasteTail) > len(end) {
		s.pasteTail = s.pasteTail[1:]
	}
	if string(s.pasteTail) == end {
		s.inPaste = false
		s.pasteTail = nil
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
