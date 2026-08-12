package subagents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestTraceContextZeroValueIsNoOp(t *testing.T) {
	var trace TraceContext
	if trace.Enabled() {
		t.Fatal("zero trace context is enabled")
	}
	trace.Record(NewTraceEvent("ignored", map[string]any{"secret": "not written"}))
	if got := TraceContextFrom(context.Background()); got.Enabled() {
		t.Fatal("context without trace is enabled")
	}
	ctx := WithTraceContext(context.Background(), trace)
	if TraceContextFrom(ctx).Enabled() {
		t.Fatal("stored no-op context is enabled")
	}
}

func TestTraceWriterCreatesPrivateBundleAndDetailedPayload(t *testing.T) {
	writer, err := NewTraceWriter(filepath.Join(t.TempDir(), "trace"), TraceModeDetailed)
	if err != nil {
		t.Fatal(err)
	}
	writer.Record(TraceEvent{Type: "tool.result", Data: map[string]any{"name": "cat"}, Payload: map[string]any{"output": "sensitive result"}})
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"manifest.json", "trace.jsonl", "payloads"} {
		if _, err := os.Stat(filepath.Join(writer.Dir(), name)); err != nil {
			t.Fatalf("bundle member %s: %v", name, err)
		}
	}
	assertPrivate(t, writer.Dir(), 0o700)
	assertPrivate(t, filepath.Join(writer.Dir(), "manifest.json"), 0o600)
	assertPrivate(t, filepath.Join(writer.Dir(), "trace.jsonl"), 0o600)

	var manifest TraceManifest
	readJSONFile(t, filepath.Join(writer.Dir(), "manifest.json"), &manifest)
	if manifest.Mode != TraceModeDetailed || manifest.TraceFile != "trace.jsonl" || manifest.Payloads != "payloads" {
		t.Fatalf("manifest = %+v", manifest)
	}
	lines := readLines(t, filepath.Join(writer.Dir(), "trace.jsonl"))
	if len(lines) != 1 {
		t.Fatalf("got %d trace lines, want 1", len(lines))
	}
	var event TraceEvent
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatal(err)
	}
	if event.Seq == 0 || event.PayloadFile == "" {
		t.Fatalf("event = %+v", event)
	}
	payloadPath := filepath.Join(writer.Dir(), filepath.FromSlash(event.PayloadFile))
	assertPrivate(t, payloadPath, 0o600)
	payload := string(mustRead(t, payloadPath))
	if !strings.Contains(payload, "sensitive result") {
		t.Fatalf("payload does not contain detailed value: %s", payload)
	}
}

func TestTraceWriterNormalRedactsSensitiveValues(t *testing.T) {
	writer, err := NewTraceWriter(filepath.Join(t.TempDir(), "trace"), TraceModeNormal)
	if err != nil {
		t.Fatal(err)
	}
	secret := "super-secret-value"
	writer.Record(TraceEvent{
		Type:    "request",
		Data:    map[string]any{"name": "safe", "authorization": secret, "nested": map[string]any{"token": secret, "count": 2}},
		Payload: map[string]any{"content": secret},
	})
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data := string(mustRead(t, filepath.Join(writer.Dir(), "trace.jsonl")))
	if strings.Contains(data, secret) || strings.Contains(data, "content") {
		t.Fatalf("normal trace leaked sensitive value: %s", data)
	}
	if !strings.Contains(data, "[REDACTED]") || !strings.Contains(data, "safe") {
		t.Fatalf("normal trace did not retain safe metadata/redaction: %s", data)
	}
	entries, err := os.ReadDir(filepath.Join(writer.Dir(), "payloads"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("normal trace wrote %d payload files", len(entries))
	}
}

func TestTraceWriterConcurrentRecordHasOrderedGlobalSequences(t *testing.T) {
	writer, err := NewTraceWriter(filepath.Join(t.TempDir(), "trace"))
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	const perWorker = 25
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for i := 0; i < perWorker; i++ {
				writer.Record(NewTraceEvent("event", map[string]any{"worker": worker, "index": i}))
			}
		}(worker)
	}
	group.Wait()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, filepath.Join(writer.Dir(), "trace.jsonl"))
	if len(lines) != workers*perWorker {
		t.Fatalf("got %d lines, want %d", len(lines), workers*perWorker)
	}
	seqs := make([]uint64, 0, len(lines))
	for _, line := range lines {
		var event TraceEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, event.Seq)
	}
	if !sort.SliceIsSorted(seqs, func(i, j int) bool { return seqs[i] < seqs[j] }) {
		t.Fatalf("trace sequences are not ordered: %v", seqs)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("sequence gap at %d: %d then %d", i, seqs[i-1], seqs[i])
		}
	}
}

func assertPrivate(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func readJSONFile(t *testing.T, path string, out any) {
	t.Helper()
	if err := json.Unmarshal(mustRead(t, path), out); err != nil {
		t.Fatal(err)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data := strings.TrimSpace(string(mustRead(t, path)))
	if data == "" {
		return nil
	}
	return strings.Split(data, "\n")
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
