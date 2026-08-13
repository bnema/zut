package subagents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

// TraceMode controls how much data a trace keeps. Normal is the safe default:
// event metadata is retained, while payloads and values under sensitive keys
// are removed. Detailed is intended for explicitly enabled local debugging.
type TraceMode string

const (
	TraceModeNormal   TraceMode = "normal"
	TraceModeDetailed TraceMode = "detailed"
)

// Normal and Detailed are short aliases useful to callers configuring a
// TraceWriter. Invalid modes are treated as Normal.
const (
	Normal   = TraceModeNormal
	Detailed = TraceModeDetailed
)

func (m TraceMode) valid() TraceMode {
	if m == TraceModeDetailed {
		return m
	}
	return TraceModeNormal
}

// TraceEvent is one entry in trace.jsonl. Payload is never embedded in the
// JSONL stream: detailed traces store it as a separate file and normal traces
// discard it. Data is for small event metadata and is recursively redacted in
// normal mode when a key identifies a secret or payload.
type TraceEvent struct {
	Seq         uint64         `json:"seq"`
	Timestamp   time.Time      `json:"timestamp"`
	Type        string         `json:"type"`
	AgentID     string         `json:"agent_id,omitempty"`
	TurnID      string         `json:"turn_id,omitempty"`
	Data        map[string]any `json:"data,omitempty"`
	Payload     any            `json:"-"`
	PayloadFile string         `json:"payload_file,omitempty"`
}

// NewTraceEvent constructs an event with a UTC timestamp. A TraceWriter
// assigns Seq when the event is recorded; callers should not assign it.
func NewTraceEvent(typ string, data map[string]any) TraceEvent {
	return TraceEvent{
		Timestamp: time.Now().UTC(),
		Type:      typ,
		Data:      data,
	}
}

// TraceManifest describes the files in a trace bundle.
type TraceManifest struct {
	Version   int       `json:"version"`
	Mode      TraceMode `json:"mode"`
	CreatedAt time.Time `json:"created_at"`
	TraceFile string    `json:"trace_file"`
	Payloads  string    `json:"payloads"`
}

// TraceWriter writes a trace bundle. Record is deliberately asynchronous: it
// only snapshots the event and adds it to an in-memory queue. Disk failures
// are retained for Close, but never returned from Record or allowed to affect
// the agent that is being traced.
type TraceWriter struct {
	mu        sync.Mutex
	cond      *sync.Cond
	pending   []traceRecord
	closed    bool
	finished  chan struct{}
	closeOnce sync.Once
	closeDone chan struct{}

	dir        string
	mode       TraceMode
	traceFile  *os.File
	payloadDir string
	memory     bool
	sequence   uint64
	dropped    uint64
	events     []TraceEvent
	projection *traceProjection

	errorMu sync.Mutex
	err     error
}

type traceRecord struct {
	event   TraceEvent
	payload []byte
	barrier chan struct{}
}

const (
	traceQueueInitialCapacity = 64
	traceQueueLimit           = 1024
	memoryTraceEventLimit     = 4096
)

// NewMemoryTraceWriter creates the non-persistent projection source used when
// bundle capture is disabled. It retains only normal-mode event metadata.
func NewMemoryTraceWriter() *TraceWriter {
	w := &TraceWriter{
		mode:      TraceModeNormal,
		memory:    true,
		pending:   make([]traceRecord, 0, traceQueueInitialCapacity),
		finished:  make(chan struct{}),
		closeDone: make(chan struct{}),
		events:    make([]TraceEvent, 0, traceQueueInitialCapacity),
	}
	w.cond = sync.NewCond(&w.mu)
	go w.run()
	return w
}

// NewTraceWriter creates a private trace bundle at dir. The directory and its
// files are created with restrictive permissions. If mode is omitted or
// invalid, TraceModeNormal is used.
func NewTraceWriter(dir string, modes ...TraceMode) (*TraceWriter, error) {
	mode := TraceModeNormal
	if len(modes) != 0 {
		mode = modes[0].valid()
	}
	if dir == "" {
		return nil, errors.New("trace writer: empty directory")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("trace writer path: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("trace writer directory: %w", err)
	}
	// MkdirAll does not tighten an existing directory's permissions.
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("trace writer directory permissions: %w", err)
	}
	if _, err := os.Stat(filepath.Join(absolute, "trace.jsonl")); err == nil {
		return nil, fmt.Errorf("trace writer: bundle already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("trace writer trace file: %w", err)
	}
	payloadDir := filepath.Join(absolute, "payloads")
	if err := os.MkdirAll(payloadDir, 0o700); err != nil {
		return nil, fmt.Errorf("trace writer payload directory: %w", err)
	}
	if err := os.Chmod(payloadDir, 0o700); err != nil {
		return nil, fmt.Errorf("trace writer payload permissions: %w", err)
	}

	traceFile, err := os.OpenFile(filepath.Join(absolute, "trace.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("trace writer trace file: %w", err)
	}
	if err := traceFile.Chmod(0o600); err != nil {
		_ = traceFile.Close()
		return nil, fmt.Errorf("trace writer trace permissions: %w", err)
	}

	manifest := TraceManifest{
		Version:   1,
		Mode:      mode,
		CreatedAt: time.Now().UTC(),
		TraceFile: "trace.jsonl",
		Payloads:  "payloads",
	}
	if err := writeTraceManifest(filepath.Join(absolute, "manifest.json"), manifest); err != nil {
		_ = traceFile.Close()
		return nil, err
	}

	w := &TraceWriter{
		dir:        absolute,
		mode:       mode,
		traceFile:  traceFile,
		payloadDir: payloadDir,
		pending:    make([]traceRecord, 0, traceQueueInitialCapacity),
		finished:   make(chan struct{}),
		closeDone:  make(chan struct{}),
		events:     make([]TraceEvent, 0, traceQueueInitialCapacity),
	}
	w.cond = sync.NewCond(&w.mu)
	go w.run()
	return w, nil
}

func writeTraceManifest(path string, manifest TraceManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("trace manifest encode: %w", err)
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("trace manifest open: %w", err)
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("trace manifest permissions: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("trace manifest write: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("trace manifest sync: %w", err)
	}
	return nil
}

// Dir returns the trace bundle directory.
func (w *TraceWriter) Dir() string {
	if w == nil {
		return ""
	}
	return w.dir
}

// Mode returns the effective mode of the bundle.
func (w *TraceWriter) Mode() TraceMode {
	if w == nil {
		return TraceModeNormal
	}
	return w.mode
}

// Record snapshots and queues an event. It never returns an error. A nil
// writer is a no-op, which makes tracing safe to leave disabled by default.
func (w *TraceWriter) Record(event TraceEvent) {
	if w == nil {
		return
	}
	record, ok := w.prepare(event)
	if !ok {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.sequence++
	record.event.Seq = w.sequence
	if w.projection == nil {
		w.projection = newTraceProjection()
	}
	// Derive live state at ingestion rather than after asynchronous disk I/O.
	// This preserves operation boundaries even when the bounded write queue
	// drops an old raw event under sustained streaming pressure.
	w.projection.apply(record.event)
	if len(w.pending) >= traceQueueLimit {
		for index, pending := range w.pending {
			if pending.barrier == nil {
				copy(w.pending[index:], w.pending[index+1:])
				w.pending[len(w.pending)-1] = traceRecord{}
				w.pending = w.pending[:len(w.pending)-1]
				w.dropped++
				w.setError(errors.New("trace event queue overflow"))
				break
			}
		}
	}
	w.pending = append(w.pending, record)
	w.cond.Signal()
}

func (w *TraceWriter) prepare(event TraceEvent) (traceRecord, bool) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	} else {
		event.Timestamp = event.Timestamp.UTC()
	}
	if w.mode == TraceModeNormal {
		event.Data = redactTraceMap(event.Data)
		event.Payload = nil
		return traceRecord{event: event}, true
	}

	event.Data = cloneTraceMap(event.Data)
	record := traceRecord{event: event}
	if event.Payload != nil {
		payload, err := json.Marshal(event.Payload)
		if err != nil {
			// Keep the metadata event even if an optional detailed payload is
			// not JSON encodable.
			w.setError(fmt.Errorf("trace payload encode: %w", err))
		} else {
			record.payload = append(payload, '\n')
		}
	}
	record.event.Payload = nil
	return record, true
}

func (w *TraceWriter) run() {
	defer close(w.finished)
	for {
		w.mu.Lock()
		for len(w.pending) == 0 && !w.closed {
			w.cond.Wait()
		}
		if len(w.pending) == 0 && w.closed {
			w.mu.Unlock()
			return
		}
		record := w.pending[0]
		w.pending[0] = traceRecord{}
		w.pending = w.pending[1:]
		w.mu.Unlock()
		w.write(record)
	}
}

func (w *TraceWriter) write(record traceRecord) {
	if record.barrier != nil {
		defer close(record.barrier)
		return
	}
	event := record.event
	if w.memory {
		w.mu.Lock()
		w.appendEvent(event)
		w.mu.Unlock()
		return
	}
	if len(record.payload) != 0 {
		name := fmt.Sprintf("%020d.json", event.Seq)
		path := filepath.Join(w.payloadDir, name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			w.setError(fmt.Errorf("trace payload file: %w", err))
		} else {
			if _, err := file.Write(record.payload); err != nil {
				w.setError(fmt.Errorf("trace payload write: %w", err))
			} else if err := file.Sync(); err != nil {
				w.setError(fmt.Errorf("trace payload sync: %w", err))
			}
			_ = file.Chmod(0o600)
			if err := file.Close(); err != nil {
				w.setError(fmt.Errorf("trace payload close: %w", err))
			}
			event.PayloadFile = filepath.ToSlash(filepath.Join("payloads", name))
		}
	}

	line, err := json.Marshal(event)
	if err != nil {
		w.setError(fmt.Errorf("trace event encode: %w", err))
		return
	}
	line = append(line, '\n')
	if _, err := w.traceFile.Write(line); err != nil {
		w.setError(fmt.Errorf("trace event write: %w", err))
		return
	}
	// Sync every event so a completed Record queue can be made durable by
	// Close, and so a crash loses at most the event currently being written.
	if err := w.traceFile.Sync(); err != nil {
		w.setError(fmt.Errorf("trace event sync: %w", err))
	}
	w.mu.Lock()
	w.appendEvent(event)
	w.mu.Unlock()
}

func (w *TraceWriter) appendEvent(event TraceEvent) {
	w.events = append(w.events, event)
	if overflow := len(w.events) - memoryTraceEventLimit; overflow > 0 {
		copy(w.events, w.events[overflow:])
		w.events = w.events[:memoryTraceEventLimit]
	}
}

// Views returns the incrementally derived trace projection. Unlike replaying
// Events, it retains open operation boundaries after old raw events are evicted
// from the bounded in-memory window.
func (w *TraceWriter) Views() map[string]AgentTraceView {
	if w == nil {
		return map[string]AgentTraceView{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.projection == nil {
		return map[string]AgentTraceView{}
	}
	return w.projection.snapshot()
}

// Events returns a stable copy of events already written by this writer. It
// never waits for queued filesystem work, keeping status and UI reads cheap.
func (w *TraceWriter) Events() []TraceEvent {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]TraceEvent(nil), w.events...)
}

func (w *TraceWriter) setError(err error) {
	if err == nil {
		return
	}
	w.errorMu.Lock()
	if w.err == nil {
		w.err = err
	}
	w.errorMu.Unlock()
}

// Flush waits until all events recorded before the call have been written.
func (w *TraceWriter) Flush() error { return w.FlushContext(context.Background()) }

// FlushContext honors cancellation while waiting for the trace writer.
func (w *TraceWriter) FlushContext(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.mu.Lock()
	wait := w.finished
	if !w.closed {
		barrier := make(chan struct{})
		w.pending = append(w.pending, traceRecord{barrier: barrier})
		w.cond.Signal()
		wait = barrier
	}
	w.mu.Unlock()
	select {
	case <-wait:
		return w.recordingError()
	case <-ctx.Done():
		return fmt.Errorf("flush trace: %w", ctx.Err())
	}
}

// Close drains the queue, syncs each event, and closes the bundle.
func (w *TraceWriter) Close() error { return w.CloseContext(context.Background()) }

// CloseContext starts closing once and honors cancellation while waiting. A
// later caller can retry the wait without repeating file-close work.
func (w *TraceWriter) CloseContext(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.closeOnce.Do(func() {
		go func() {
			w.mu.Lock()
			w.closed = true
			w.cond.Broadcast()
			w.mu.Unlock()
			<-w.finished
			if !w.memory {
				if err := w.traceFile.Close(); err != nil {
					w.setError(fmt.Errorf("trace file close: %w", err))
				}
			}
			close(w.closeDone)
		}()
	})
	select {
	case <-w.closeDone:
		return w.recordingError()
	case <-ctx.Done():
		return fmt.Errorf("close trace: %w", ctx.Err())
	}
}

func (w *TraceWriter) recordingError() error {
	w.errorMu.Lock()
	defer w.errorMu.Unlock()
	return w.err
}

// TraceContext is the opt-in handle passed through agent code. Its zero value
// is a no-op, so callers do not need conditionals when tracing is disabled.
type TraceContext struct {
	writer *TraceWriter
}

// NewTraceContext returns an enabled context for writer. A nil writer creates
// a no-op context. The writer's mode is authoritative.
func NewTraceContext(writer *TraceWriter) TraceContext {
	return TraceContext{writer: writer}
}

// Context returns an enabled TraceContext for this writer.
func (w *TraceWriter) Context() TraceContext { return NewTraceContext(w) }

// Enabled reports whether this context will record events.
func (t TraceContext) Enabled() bool { return t.writer != nil }

// Mode returns the writer mode, or normal for a no-op context.
func (t TraceContext) Mode() TraceMode {
	if t.writer == nil {
		return TraceModeNormal
	}
	return t.writer.Mode()
}

// Record records an event when tracing is enabled. The no-op path is safe for
// a zero TraceContext.
func (t TraceContext) Record(event TraceEvent) {
	if t.writer != nil {
		t.writer.Record(event)
	}
}

// WithTraceContext stores a trace handle in a standard context.Context.
func WithTraceContext(ctx context.Context, trace TraceContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, traceContextKey{}, trace)
}

type traceContextKey struct{}

// TraceContextFrom retrieves a trace handle, returning a no-op context when
// none was installed.
func TraceContextFrom(ctx context.Context) TraceContext {
	if ctx == nil {
		return TraceContext{}
	}
	trace, _ := ctx.Value(traceContextKey{}).(TraceContext)
	return trace
}

// RecordTrace records through the context's trace handle and is a convenient
// best-effort call site for code that does not need to retain the handle.
func RecordTrace(ctx context.Context, event TraceEvent) {
	TraceContextFrom(ctx).Record(event)
}

var sensitiveTraceKeys = map[string]struct{}{
	"api_key": {}, "apikey": {}, "authorization": {}, "auth": {},
	"certificate": {}, "cookie": {}, "credential": {}, "credentials": {},
	"password": {}, "private_key": {}, "secret": {}, "session": {},
	"token": {}, "access_token": {}, "refresh_token": {}, "payload": {},
	"prompt": {}, "input": {}, "output": {}, "content": {}, "message": {},
	"text": {}, "body": {}, "data": {}, "result": {}, "results": {},
	"request": {}, "response": {}, "arguments": {}, "args": {}, "query": {},
	"value": {}, "values": {}, "raw": {}, "stdout": {}, "stderr": {},
}

func redactTraceMap(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	out := make(map[string]any, len(data))
	for key, value := range data {
		if sensitiveTraceKey(normalizeTraceKey(key)) {
			out[key] = "[REDACTED]"
			continue
		}
		out[key] = redactTraceValue(value)
	}
	return out
}

func sensitiveTraceKey(key string) bool {
	for fragment := range sensitiveTraceKeys {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
}

func redactTraceValue(value any) any {
	if value == nil {
		return nil
	}
	switch value := value.(type) {
	case map[string]any:
		return redactTraceMap(value)
	case []any:
		out := make([]any, len(value))
		for i := range value {
			out[i] = redactTraceValue(value[i])
		}
		return out
	}
	v := reflect.ValueOf(value)
	if v.Kind() == reflect.Map && v.Type().Key().Kind() == reflect.String {
		out := make(map[string]any, v.Len())
		iter := v.MapRange()
		for iter.Next() {
			key := iter.Key().String()
			if sensitiveTraceKey(normalizeTraceKey(key)) {
				out[key] = "[REDACTED]"
			} else {
				out[key] = redactTraceValue(iter.Value().Interface())
			}
		}
		return out
	}
	if v.Kind() == reflect.Struct {
		encoded, err := json.Marshal(value)
		if err == nil {
			var data map[string]any
			if json.Unmarshal(encoded, &data) == nil {
				return redactTraceMap(data)
			}
		}
	}
	return value
}

func cloneTraceMap(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	out := make(map[string]any, len(data))
	for key, value := range data {
		out[key] = cloneTraceValue(value)
	}
	return out
}

func cloneTraceValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneTraceMap(value)
	case []any:
		out := make([]any, len(value))
		for i := range value {
			out[i] = cloneTraceValue(value[i])
		}
		return out
	default:
		return value
	}
}

func normalizeTraceKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "-", "_")
	key = strings.ReplaceAll(key, " ", "_")
	return key
}
