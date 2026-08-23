package tui

import (
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// DetectTrueColor reports whether TERM or COLORTERM advertises direct color
// support. This follows the same conservative signals used by ../vev.
func DetectTrueColor(termEnv, colorTerm string) bool {
	switch strings.ToLower(strings.TrimSpace(colorTerm)) {
	case "truecolor", "24bit":
		return true
	}

	termEnv = strings.ToLower(strings.TrimSpace(termEnv))
	return termEnv == "xterm-direct" || strings.HasSuffix(termEnv, "-direct")
}

// DetectColorDepth conservatively chooses the highest color mode advertised
// by the environment. Terminals without a 256-color signal stay ANSI16.
func DetectColorDepth(termEnv, colorTerm string) ColorDepth {
	if DetectTrueColor(termEnv, colorTerm) {
		return ColorDepthTrueColor
	}
	termEnv = strings.ToLower(strings.TrimSpace(termEnv))
	if strings.Contains(termEnv, "256color") {
		return ColorDepthIndexed256
	}
	return ColorDepthANSI16
}

// DetectThemeFromBackground queries the controlling tty for its current
// foreground, background, and ANSI palette using OSC 10/11/4. Auto is always
// terminal-owned; the snapshot is retained for subsequent runtime resolution.
//
// The query / parse runs synchronously before the TUI is initialised so the
// returned snapshot can drive the entire session. We briefly put stdin into
// raw mode and disable echo so OSC replies do not leak onto the user's screen.
func DetectThemeFromBackground(timeout time.Duration) Theme {
	return TerminalTheme(detectTerminalProfile(timeout))
}

type oscColorResponse struct {
	kind  int
	slot  int
	color TerminalColor
}

func detectTerminalProfile(timeout time.Duration) TerminalProfile {
	profile := TerminalProfile{Depth: DetectColorDepth(os.Getenv("TERM"), os.Getenv("COLORTERM")), TrueColor: DetectTrueColor(os.Getenv("TERM"), os.Getenv("COLORTERM"))}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) || timeout <= 0 {
		return profile
	}

	fd := int(os.Stdin.Fd())
	st, err := term.MakeRaw(fd)
	if err != nil {
		return profile
	}
	defer term.Restore(fd, st)

	queries := terminalColorQueries()
	if _, err := os.Stdout.Write([]byte(queries)); err != nil {
		return profile
	}

	responses := readOSCColorResponses(time.Now().Add(timeout), terminalColorQueryCount)
	for _, response := range responses {
		switch response.kind {
		case 10:
			profile.Foreground = response.color
			profile.HasForeground = true
		case 11:
			profile.Background = response.color
			profile.HasBackground = true
		case 4:
			if response.slot >= 0 && response.slot < len(profile.Palette) {
				profile.Palette[response.slot] = response.color
				profile.PaletteKnown |= uint16(1) << response.slot
			}
		}
	}
	if profile.HasBackground {
		// This is a branch-selection fallback, not a reported scheme. Runtime
		// CSI 997 replies alone set SchemeKnown.
		profile.Light = 0.2126*float64(profile.Background.R)+
			0.7152*float64(profile.Background.G)+
			0.0722*float64(profile.Background.B) >= 127.5
	}
	return profile
}

const (
	terminalColorQueryCount    = 18
	terminalColorResponseLimit = 64
)

func terminalColorQueries() string {
	var b strings.Builder
	b.Grow(terminalColorQueryCount * 12)
	b.WriteString("\x1b]10;?\x07\x1b]11;?\x07")
	for slot := 0; slot < 16; slot++ {
		b.WriteString("\x1b]4;")
		b.WriteString(itoa(slot))
		b.WriteString(";?\x07")
	}
	return b.String()
}

// readOSCColorResponses drains terminal replies until all expected replies
// arrive, the deadline expires, or stdin hits EOF. It only consumes complete
// BEL/ST-terminated OSC color responses; an incomplete tail is discarded at
// timeout so a failed query cannot poison the next interactive read.
func readOSCColorResponses(deadline time.Time, expected int) []oscColorResponse {
	responses := make([]oscColorResponse, 0, expected)
	buf := make([]byte, 0, 64)
	for len(responses) < expected && time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		b, ok, err := peekStdin(os.Stdin, remaining)
		if err != nil || !ok {
			break
		}
		if len(buf) >= terminalColorResponseLimit {
			buf = buf[:0]
		}
		buf = append(buf, b)
		terminated := b == '\x07' || (len(buf) >= 2 && buf[len(buf)-2] == '\x1b' && buf[len(buf)-1] == '\\')
		if !terminated {
			continue
		}
		if response, ok := parseOSCColorResponse(buf); ok {
			responses = append(responses, response)
		}
		buf = buf[:0]
	}
	return responses
}

func parseOSCColorResponse(raw []byte) (oscColorResponse, bool) {
	s := string(raw)
	s = strings.TrimSuffix(s, "\x07")
	s = strings.TrimSuffix(s, "\x1b\\")

	if strings.HasPrefix(s, "\x1b]10;") {
		color, ok := parseTerminalRGB(s[len("\x1b]10;"):])
		return oscColorResponse{kind: 10, color: color}, ok
	}
	if strings.HasPrefix(s, "\x1b]11;") {
		color, ok := parseTerminalRGB(s[len("\x1b]11;"):])
		return oscColorResponse{kind: 11, color: color}, ok
	}
	if strings.HasPrefix(s, "\x1b]4;") {
		body := s[len("\x1b]4;"):]
		parts := strings.SplitN(body, ";", 2)
		if len(parts) != 2 || parts[0] == "" {
			return oscColorResponse{}, false
		}
		slot, err := strconv.Atoi(parts[0])
		if err != nil || slot < 0 || slot >= 16 {
			return oscColorResponse{}, false
		}
		color, ok := parseTerminalRGB(parts[1])
		return oscColorResponse{kind: 4, slot: slot, color: color}, ok
	}
	return oscColorResponse{}, false
}

func parseTerminalRGB(value string) (TerminalColor, bool) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "#") {
		if len(value) != 7 {
			return TerminalColor{}, false
		}
		r, ok := parseHexByte(value[1:3])
		if !ok {
			return TerminalColor{}, false
		}
		g, ok := parseHexByte(value[3:5])
		if !ok {
			return TerminalColor{}, false
		}
		b, ok := parseHexByte(value[5:7])
		if !ok {
			return TerminalColor{}, false
		}
		return ColorRGB(r, g, b), true
	}
	if !strings.HasPrefix(value, "rgb:") {
		return TerminalColor{}, false
	}
	parts := strings.Split(value[len("rgb:"):], "/")
	if len(parts) != 3 {
		return TerminalColor{}, false
	}
	var channels [3]int
	for i, part := range parts {
		if len(part) < 1 || len(part) > 4 {
			return TerminalColor{}, false
		}
		v, err := strconv.ParseUint(part, 16, 16)
		if err != nil {
			return TerminalColor{}, false
		}
		max := uint64(1)
		for range part {
			max *= 16
		}
		max--
		channels[i] = int(v * 255 / max)
	}
	return ColorRGB(channels[0], channels[1], channels[2]), true
}
