package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// FileDeferredStore keeps review actions outside the core transcript. Its path is
// supplied by assembly; NewFileDeferredStore derives the daemon-owned location.
type FileDeferredStore struct {
	mu      sync.Mutex
	path    string
	actions []DeferredAction
}

// NewFileDeferredStore loads $ZUT_HOME/daemon/sessions/<id>/deferred.jsonl.
func NewFileDeferredStore(home string, id ID) (*FileDeferredStore, error) {
	if id == "" || filepath.Base(string(id)) != string(id) || id == "." || id == ".." {
		return nil, errors.New("invalid deferred session ID")
	}
	s := &FileDeferredStore{path: filepath.Join(home, "daemon", "sessions", string(id), "deferred.jsonl")}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(f)
	for {
		var d DeferredAction
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode deferred actions: %w", err)
		}
		s.actions = append(s.actions, d)
	}
	return s, nil
}

// Append durably appends one action before exposing it in List.
func (s *FileDeferredStore) Append(d DeferredAction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(d)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	s.actions = append(s.actions, confirmCopy(d))
	return nil
}

// List returns an independent copy of persisted actions.
func (s *FileDeferredStore) List() []DeferredAction {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.actions)
	var out []DeferredAction
	_ = json.Unmarshal(b, &out)
	return out
}

// Remove compacts through a private temporary file and atomic rename.
func (s *FileDeferredStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make([]DeferredAction, 0, len(s.actions))
	found := false
	for _, d := range s.actions {
		if d.ID == id {
			found = true
			continue
		}
		next = append(next, d)
	}
	if !found {
		return ErrUnknownRequest
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".deferred-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	enc := json.NewEncoder(f)
	for _, d := range next {
		if err = enc.Encode(d); err != nil {
			break
		}
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	s.actions = next
	return nil
}
