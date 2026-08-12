package subagents

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ReadTrace reads the stable JSONL event stream from a trace bundle.
func ReadTrace(bundle string) ([]TraceEvent, error) {
	file, err := os.Open(filepath.Join(bundle, "trace.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("open trace: %w", err)
	}
	defer file.Close()
	var events []TraceEvent
	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 4*1024*1024)
	for scanner.Scan() {
		var event TraceEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("decode trace event: %w", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read trace: %w", err)
	}
	return events, nil
}
