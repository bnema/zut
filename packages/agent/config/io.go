package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Load reads the config file, returning defaults if missing.
func Load() (Config, error) {
	var c Config
	b, err := os.ReadFile(Path())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse config: %w", err)
	}
	if err := validateDictationSettings(c.Dictation); err != nil {
		return c, err
	}
	if err := validateSubagentConfig(c.Subagents); err != nil {
		return c, err
	}
	if err := validateAdvisorConfig(c.Advisor); err != nil {
		return c, err
	}
	return c, nil
}

// LoadLocked waits for any in-flight Update or Save in this process, then
// loads under the same lock so a reload never reads partway through a
// read-modify-write sequence.
func LoadLocked() (Config, error) {
	mu.Lock()
	defer mu.Unlock()
	return Load()
}

func validateSubagentConfig(cfg SubagentsConfig) error {
	values := []struct {
		name  string
		value string
	}{
		{"queue_timeout", cfg.QueueTimeout},
	}
	for _, item := range values {
		value := strings.TrimSpace(item.value)
		if value == "" {
			continue
		}
		if _, err := time.ParseDuration(value); err != nil {
			return fmt.Errorf("parse config: subagents.%s: %w", item.name, err)
		}
	}
	return nil
}

// mu serialises every in-process read-modify-write of config.json, so
// concurrent daemon sessions never lose each other's updates.
var mu sync.Mutex

// ErrUnchanged lets an Update callback skip the write.
var ErrUnchanged = errors.New("config unchanged")

// Update loads config.json, applies fn, and saves the result under the package
// lock. The callback runs under that lock and must not call LoadLocked, Save,
// or Update, which would deadlock. It may return ErrUnchanged to skip saving.
func Update(fn func(*Config) error) error {
	mu.Lock()
	defer mu.Unlock()
	cfg, err := Load()
	if err != nil {
		return err
	}
	if err := fn(&cfg); err != nil {
		if errors.Is(err, ErrUnchanged) {
			return nil
		}
		return err
	}
	return writeConfig(cfg)
}

// Save writes the config file, creating parent dirs. Read-modify-write
// callers use Update instead.
func Save(c Config) error {
	mu.Lock()
	defer mu.Unlock()
	return writeConfig(c)
}

// writeConfig replaces config.json atomically: readers see the old or the
// new file, never a partial one.
func writeConfig(c Config) error {
	if err := validateDictationSettings(c.Dictation); err != nil {
		return err
	}
	if err := os.MkdirAll(ZutHome(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(Path(), b, 0o600)
}

// writeFileAtomic writes data to a temp file beside path, syncs it and
// renames it over path.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if name != "" {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	name = ""
	return nil
}
