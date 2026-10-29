package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/bnema/zut/packages/agent/session"
)

const stateVersion = 1

// persistedState is $ZUT_HOME/daemon/state.json: the sessions that are open
// and must be restored idle after a daemon restart (D6). Env is captured from
// the creating client and may be sensitive; the file is 0600 in a 0700 dir.
type persistedState struct {
	Version  int            `json:"version"`
	Sessions []stateSession `json:"sessions"`
}

type stateSession struct {
	ID          session.ID `json:"id"`
	SessionPath string     `json:"session_path"`
	CWD         string     `json:"cwd"`
	Env         []string   `json:"env"`
	Args        []string   `json:"args"`
	Headless    bool       `json:"headless,omitempty"`
	// Zutfile keeps packaged-agent permissions across restore and clear.
	Zutfile *session.ZutfileSpec `json:"zutfile,omitempty"`
	// Running records that a turn was in flight at the last state write; the
	// restore marks it interrupted.
	Running bool `json:"running,omitempty"`
}

func statePath(home string) string { return filepath.Join(stateDir(home), "state.json") }

func loadState(home string) (persistedState, error) {
	b, err := os.ReadFile(statePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return persistedState{Version: stateVersion}, nil
	}
	if err != nil {
		return persistedState{}, fmt.Errorf("read daemon state: %w", err)
	}
	var st persistedState
	if err := json.Unmarshal(b, &st); err != nil {
		return persistedState{}, fmt.Errorf("decode daemon state: %w", err)
	}
	return st, nil
}

func saveState(home string, st persistedState) error {
	st.Version = stateVersion
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode daemon state: %w", err)
	}
	return writeFileAtomic(statePath(home), append(b, '\n'))
}

// writeFileAtomic writes a 0600 file via temp+rename in a 0700 directory and
// fsyncs the parent directory on Unix so the rename is durable.
func writeFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename state file: %w", err)
	}
	if runtime.GOOS != "windows" {
		d, derr := os.Open(dir)
		if derr != nil {
			return fmt.Errorf("open state dir: %w", derr)
		}
		serr := d.Sync()
		_ = d.Close()
		if serr != nil {
			return fmt.Errorf("sync state dir: %w", serr)
		}
	}
	return nil
}

func sortState(st *persistedState) {
	sort.Slice(st.Sessions, func(i, j int) bool { return st.Sessions[i].ID < st.Sessions[j].ID })
}
