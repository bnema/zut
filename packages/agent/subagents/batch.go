package subagents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// BatchRequest starts independent child tasks under one aggregate id. Batch
// scheduling is deliberately shallow; dependency graphs belong in workflows.
type BatchRequest struct {
	Context       string
	Tasks         []string
	MaxConcurrent int
	Spawn         SpawnRequest
}

type BatchStatus string

const (
	BatchQueued    BatchStatus = "queued"
	BatchRunning   BatchStatus = "running"
	BatchSucceeded BatchStatus = "succeeded"
	BatchFailed    BatchStatus = "failed"
	BatchCanceled  BatchStatus = "canceled"
)

type BatchResult struct {
	Version   int                    `json:"version"`
	BatchID   string                 `json:"batch_id"`
	TaskCount int                    `json:"task_count,omitempty"`
	Status    BatchStatus            `json:"status"`
	ChildIDs  []string               `json:"child_ids"`
	Results   map[string]*TurnResult `json:"results,omitempty"`
	Error     string                 `json:"error,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
}

const maxBatchFileBytes = 32 * 1024 * 1024

type Batch struct {
	ID            string
	Context       string
	ChildIDs      []string
	MaxConcurrent int
	TaskCount     int
	CreatedAt     time.Time

	mu     sync.Mutex
	status BatchStatus
	result *BatchResult
	done   chan struct{}
	once   sync.Once
}

func (b *Batch) Status() BatchStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

func (b *Batch) Result() *BatchResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	return cloneBatchResult(b.result)
}

func (b *Batch) Wait()      { <-b.done }
func (b *Batch) closeDone() { b.once.Do(func() { close(b.done) }) }

func (b *Batch) WaitContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *Supervisor) SpawnBatch(ctx context.Context, req BatchRequest) (*Batch, error) {
	if len(req.Tasks) == 0 {
		return nil, fmt.Errorf("subagents: batch requires at least one task")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := f.cfg.Now()
	baseID := newAgentID("batch "+now.Format("150405.000"), now)
	f.mu.Lock()
	if f.batches == nil {
		f.batches = make(map[string]*Batch)
	}
	batchID := baseID
	for suffix := 2; ; suffix++ {
		if _, exists := f.batches[batchID]; !exists {
			break
		}
		batchID = fmt.Sprintf("%s-%d", baseID, suffix)
	}
	batch := &Batch{
		ID:            batchID,
		Context:       strings.TrimSpace(req.Context),
		MaxConcurrent: req.MaxConcurrent,
		TaskCount:     len(req.Tasks),
		CreatedAt:     now,
		status:        BatchQueued,
		result: &BatchResult{
			Version: ProtocolVersion, BatchID: batchID, TaskCount: len(req.Tasks), Status: BatchQueued,
			CreatedAt: now, UpdatedAt: now,
		},
		done: make(chan struct{}),
	}
	f.batches[batch.ID] = batch
	f.mu.Unlock()
	// Write a queued manifest before admitting children so a crash during
	// batch admission still leaves an aggregate id to reload and reconcile.
	f.persistBatch(batch)

	for _, task := range req.Tasks {
		spawnReq := req.Spawn
		spawnReq.Task = task
		spawnReq.ParentID = batch.ID
		spawnReq.BatchID = batch.ID
		a, err := f.SpawnReq(ctx, spawnReq)
		if err != nil {
			childIDs := append([]string(nil), batch.ChildIDs...)
			for _, childID := range childIDs {
				_ = f.Stop(childID)
			}
			for _, childID := range childIDs {
				if child := f.Get(childID); child != nil {
					child.Wait()
				}
			}
			batch.mu.Lock()
			batch.status = BatchFailed
			batch.result = &BatchResult{
				Version: ProtocolVersion, BatchID: batch.ID, TaskCount: batch.TaskCount, Status: BatchFailed,
				ChildIDs: childIDs, Error: err.Error(), CreatedAt: now, UpdatedAt: f.cfg.Now(),
			}
			batch.mu.Unlock()
			f.persistBatch(batch)
			batch.closeDone()
			return batch, err
		}
		batch.ChildIDs = append(batch.ChildIDs, a.ID)
		batch.mu.Lock()
		batch.result = &BatchResult{
			Version: ProtocolVersion, BatchID: batch.ID, TaskCount: batch.TaskCount, Status: BatchQueued,
			ChildIDs: append([]string(nil), batch.ChildIDs...), CreatedAt: batch.CreatedAt,
			UpdatedAt: f.cfg.Now(),
		}
		batch.mu.Unlock()
		f.persistBatch(batch)
	}
	batch.mu.Lock()
	batch.status = BatchRunning
	batch.mu.Unlock()
	f.persistBatch(batch)
	go f.collectBatch(batch)
	return batch, nil
}

func (f *Supervisor) collectBatch(batch *Batch) {
	if batch.TaskCount > 0 && len(batch.ChildIDs) < batch.TaskCount {
		batch.mu.Lock()
		batch.status = BatchFailed
		batch.result = &BatchResult{
			Version: ProtocolVersion, BatchID: batch.ID, TaskCount: batch.TaskCount,
			Status: BatchFailed, ChildIDs: append([]string(nil), batch.ChildIDs...),
			Error: "batch admission was incomplete", CreatedAt: batch.CreatedAt, UpdatedAt: f.cfg.Now(),
		}
		batch.mu.Unlock()
		f.persistBatch(batch)
		batch.closeDone()
		return
	}
	results := make(map[string]*TurnResult, len(batch.ChildIDs))
	status := BatchSucceeded
	var firstErr string
	for _, id := range batch.ChildIDs {
		a := f.Get(id)
		if a == nil {
			status = BatchFailed
			if firstErr == "" {
				firstErr = "missing child " + id
			}
			continue
		}
		result, err := f.waitForBatchResult(f.lifetimeCtx, a)
		if err == nil {
			results[id] = result
			switch result.Status {
			case ResultFailed:
				if status == BatchSucceeded {
					status = BatchFailed
					if result.Error != nil {
						firstErr = result.Error.Message
					}
				}
			case ResultCanceled:
				if status == BatchSucceeded {
					status = BatchCanceled
					firstErr = "one or more batch children were canceled"
				}
			}
		} else {
			status = BatchFailed
			if firstErr == "" {
				firstErr = err.Error()
			}
		}
	}
	batch.mu.Lock()
	batch.status = status
	batch.result = &BatchResult{
		Version: ProtocolVersion, BatchID: batch.ID, TaskCount: batch.TaskCount, Status: status,
		ChildIDs: append([]string(nil), batch.ChildIDs...), Results: results,
		Error: firstErr, CreatedAt: batch.CreatedAt, UpdatedAt: f.cfg.Now(),
	}
	batch.mu.Unlock()
	f.persistBatch(batch)
	batch.closeDone()
}

func (f *Supervisor) waitForBatchResult(ctx context.Context, a *Agent) (*TurnResult, error) {
	if a == nil {
		return nil, errors.New("subagents: missing batch child")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if result := a.Result(); result != nil {
		return result, nil
	}
	if a.Status() != StatusDetached {
		return a.waitForTurnResult(ctx)
	}

	// A reloaded worker has a closed supervisor-side done channel even when
	// its orphan process is still alive. Reconcile durable result.json/events
	// before treating that closed channel as a failed child.
	readDurable := func() *TurnResult {
		if result, err := readTurnResult(a.stateDirectory(f.cfg.Root)); err == nil && validateTurnResultAgent(result, a.ID) == nil {
			result = result.Bounded(f.cfg.Policy.MaxOutputBytes, f.cfg.Policy.MaxOutputLines)
			a.setResult(result)
			return result
		}
		events, err := ReadEventLog(a.EventLogPath)
		if err != nil {
			return nil
		}
		for _, event := range events {
			if event.Type != EventTurnResult && event.Type != "turn_result" {
				continue
			}
			result, decodeErr := decodeTurnResultEvent(event, a.ID, f.cfg.Policy.MaxOutputBytes, f.cfg.Policy.MaxOutputLines)
			if decodeErr != nil {
				continue
			}
			a.setResult(result)
			return result
		}
		return nil
	}

	if result := readDurable(); result != nil {
		return result, nil
	}
	if !inboxLive(a.InboxPath) {
		return nil, fmt.Errorf("subagents: agent %s exited without a turn result", a.ID)
	}

	interval := 100 * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if result := readDurable(); result != nil {
				return result, nil
			}
			if !inboxLive(a.InboxPath) {
				return nil, fmt.Errorf("subagents: agent %s exited without a turn result", a.ID)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (f *Supervisor) WaitBatch(id string) (*BatchResult, error) {
	return f.WaitBatchContext(context.Background(), id)
}

// WaitBatchContext waits for the aggregate result without changing the batch
// or any child when the observer's context ends.
func (f *Supervisor) WaitBatchContext(ctx context.Context, id string) (*BatchResult, error) {
	f.mu.Lock()
	batch := f.batches[id]
	f.mu.Unlock()
	if batch == nil {
		return nil, fmt.Errorf("subagents: no such batch %q", id)
	}
	if err := batch.WaitContext(ctx); err != nil {
		return nil, err
	}
	return batch.Result(), nil
}

func (f *Supervisor) GetBatch(id string) *Batch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.batches[id]
}

func cloneBatchResult(result *BatchResult) *BatchResult {
	if result == nil {
		return nil
	}
	copy := *result
	copy.ChildIDs = append([]string(nil), result.ChildIDs...)
	if result.Results != nil {
		copy.Results = make(map[string]*TurnResult, len(result.Results))
		for id, child := range result.Results {
			copy.Results[id] = cloneTurnResult(child)
		}
	}
	return &copy
}

func (f *Supervisor) reloadBatches(root string) []error {
	entries, err := os.ReadDir(filepath.Join(root, "batches"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return []error{fmt.Errorf("subagents reload batches %s: %w", root, err)}
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		fileID := strings.TrimSuffix(entry.Name(), ".json")
		if !safeAgentID(fileID) {
			errs = append(errs, fmt.Errorf("subagents reload batch %s: unsafe batch id", entry.Name()))
			continue
		}
		path := filepath.Join(root, "batches", entry.Name())
		file, openErr := os.Open(path)
		if openErr != nil {
			errs = append(errs, fmt.Errorf("subagents reload batch %s: %w", path, openErr))
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxBatchFileBytes+1))
		_ = file.Close()
		if readErr != nil {
			errs = append(errs, fmt.Errorf("subagents reload batch %s: %w", path, readErr))
			continue
		}
		if len(data) > maxBatchFileBytes {
			errs = append(errs, fmt.Errorf("subagents reload batch %s: exceeds %d bytes", path, maxBatchFileBytes))
			continue
		}
		var result BatchResult
		if unmarshalErr := json.Unmarshal(data, &result); unmarshalErr != nil {
			errs = append(errs, fmt.Errorf("subagents reload batch %s: %w", path, unmarshalErr))
			continue
		}
		if result.Version == 0 {
			result.Version = ProtocolVersion
		}
		if result.Version != ProtocolVersion || result.BatchID != fileID || !safeAgentID(result.BatchID) || result.TaskCount < 0 || len(result.ChildIDs) > result.TaskCount {
			errs = append(errs, fmt.Errorf("subagents reload batch %s: invalid result metadata", path))
			continue
		}
		batch := &Batch{
			ID:        result.BatchID,
			ChildIDs:  append([]string(nil), result.ChildIDs...),
			TaskCount: result.TaskCount,
			CreatedAt: result.CreatedAt,
			status:    result.Status,
			result:    cloneBatchResult(&result),
			done:      make(chan struct{}),
		}
		if batch.CreatedAt.IsZero() {
			batch.CreatedAt = result.UpdatedAt
		}
		f.mu.Lock()
		_, exists := f.batches[batch.ID]
		if !exists {
			f.batches[batch.ID] = batch
		}
		f.mu.Unlock()
		if exists {
			continue
		}
		switch batch.status {
		case BatchSucceeded, BatchFailed, BatchCanceled:
			batch.closeDone()
		default:
			go f.collectBatch(batch)
		}
	}
	return errs
}

func (f *Supervisor) persistBatch(batch *Batch) {
	if batch == nil || !safeAgentID(batch.ID) {
		return
	}
	result := batch.Result()
	if result == nil {
		return
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil || len(data) > maxBatchFileBytes {
		return
	}
	dir := filepath.Join(f.cfg.Root, "batches")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	path := filepath.Join(dir, batch.ID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}
