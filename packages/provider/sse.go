package provider

import (
	"bufio"
	"io"
	"strings"
)

// sseEvent is one parsed event from a text/event-stream.
type sseEvent struct {
	Event string // value of "event:" field (may be empty)
	Data  string // concatenated "data:" lines
	Err   error  // terminal scanner failure, if any
}

// readSSE reads events from r and sends them on out. A scanner failure is
// delivered as a final event before out closes.
func readSSE(r io.Reader, out chan<- sseEvent) {
	defer close(out)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var ev sseEvent
	flush := func() {
		if ev.Data == "" && ev.Event == "" {
			return
		}
		out <- ev
		ev = sseEvent{}
	}

	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			// comment / keep-alive
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			field = line
			value = ""
		}
		// optional single leading space after ':'
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.Event = value
		case "data":
			if ev.Data != "" {
				ev.Data += "\n"
			}
			ev.Data += value
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		out <- sseEvent{Err: err}
	}
}
