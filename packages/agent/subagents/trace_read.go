package subagents

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadTrace reads a trace bundle using a background context.
func ReadTrace(bundle string) ([]TraceEvent, error) {
	return ReadTraceContext(context.Background(), bundle)
}

// ReadTraceContext reads the manifest-selected JSONL trace stream. If a stream
// ends with malformed data, events decoded before it are returned with the
// error so callers can still diagnose the valid prefix.
func ReadTraceContext(ctx context.Context, bundle string) ([]TraceEvent, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	manifestPath := filepath.Join(bundle, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read trace manifest: %w", err)
	}
	var manifest TraceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode trace manifest: %w", err)
	}
	if manifest.Version != 1 {
		return nil, fmt.Errorf("unsupported trace manifest version %d", manifest.Version)
	}
	tracePath, err := traceBundlePath(bundle, manifest.TraceFile)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(tracePath)
	if err != nil {
		return nil, fmt.Errorf("open trace: %w", err)
	}
	defer file.Close()

	var events []TraceEvent
	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 4*1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return events, fmt.Errorf("read trace: %w", err)
		}
		var event TraceEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return events, fmt.Errorf("decode trace event: %w", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return events, fmt.Errorf("read trace: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return events, fmt.Errorf("read trace: %w", err)
	}
	return events, nil
}

func traceBundlePath(bundle, name string) (string, error) {
	if strings.TrimSpace(name) == "" || filepath.IsAbs(name) {
		return "", errors.New("invalid trace manifest path")
	}
	root, err := filepath.Abs(bundle)
	if err != nil {
		return "", fmt.Errorf("resolve trace bundle: %w", err)
	}
	path, err := filepath.Abs(filepath.Join(root, name))
	if err != nil {
		return "", fmt.Errorf("resolve trace path: %w", err)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("trace manifest path escapes bundle")
	}
	return path, nil
}
