package tui

import (
	"io"
	"testing"
	"time"
)

func TestParseAppearanceOSC(t *testing.T) {
	events := ParseAppearanceOSC([]byte("\x1b]4;1;rgb:ff/00/00;12;#123456\x1b\\"))
	if len(events) != 2 {
		t.Fatalf("events = %#v, want two palette entries", events)
	}
	if events[0].Slot != 1 || events[0].Color != ColorRGB(255, 0, 0) {
		t.Fatalf("first event = %#v", events[0])
	}
	if events[1].Slot != 12 || events[1].Color != ColorRGB(0x12, 0x34, 0x56) {
		t.Fatalf("second event = %#v", events[1])
	}
}

func TestParseAppearanceCSI(t *testing.T) {
	scheme, mode := ParseAppearanceCSI([]byte("\x1b[?997;2n"))
	if scheme == nil || !scheme.Light || mode != nil {
		t.Fatalf("scheme parse = %#v, %#v", scheme, mode)
	}
	scheme, mode = ParseAppearanceCSI([]byte("\x1b[?2031;2$y"))
	if scheme != nil || mode == nil || mode.Status != 2 {
		t.Fatalf("mode parse = %#v, %#v", scheme, mode)
	}
}

type appearanceBytes struct{ data []byte }

func (s *appearanceBytes) read() (byte, error) {
	if len(s.data) == 0 {
		return 0, io.EOF
	}
	b := s.data[0]
	s.data = s.data[1:]
	return b, nil
}

func (s *appearanceBytes) peek(time.Duration) (byte, bool, error) {
	if len(s.data) == 0 {
		return 0, false, nil
	}
	b := s.data[0]
	s.data = s.data[1:]
	return b, true, nil
}

func TestAppearanceSourceConsumesOnlyPendingReplies(t *testing.T) {
	input := &appearanceBytes{data: []byte("a\x1b]11;#123456\ab")}
	parser := &AppearanceParser{}
	parser.SetPendingColors(true)
	var events []InputEvent
	source := NewAppearanceSource(input.read, input.peek, parser, func(event InputEvent) { events = append(events, event) })
	var got []byte
	for {
		b, err := source.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, b)
	}
	if string(got) != "ab" {
		t.Fatalf("remaining bytes = %q, want ab", got)
	}
	if len(events) != 1 || events[0].Color == nil || events[0].Color.Kind != 11 {
		t.Fatalf("events = %#v", events)
	}
}

func TestAppearanceSourceLeavesPasteOpaque(t *testing.T) {
	input := &appearanceBytes{data: []byte("\x1b[200~x\x1b]11;#123456\ay\x1b[201~")}
	parser := &AppearanceParser{}
	parser.SetPendingColors(true)
	var events []InputEvent
	source := NewAppearanceSource(input.read, input.peek, parser, func(event InputEvent) { events = append(events, event) })
	reader := NewReader(source.ReadByte)
	key, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if key.Kind != KeyPaste || key.Paste != "x\x1b]11;#123456\ay" {
		t.Fatalf("paste = %#v", key)
	}
	if len(events) != 0 {
		t.Fatalf("appearance events leaked from paste: %#v", events)
	}
}
