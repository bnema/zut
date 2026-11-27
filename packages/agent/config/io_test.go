package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSaveAtomicReplace uses a real goroutine and real file I/O on purpose:
// it checks that a concurrent reader never sees a partial config.json, which
// depends on the OS rename, not on scheduling that synctest could control.
func TestSaveAtomicReplace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	big := Config{QuickModelShortcuts: make([]QuickModelShortcut, 9)}
	for i := range big.QuickModelShortcuts {
		big.QuickModelShortcuts[i] = QuickModelShortcut{Provider: "openai", Model: strings.Repeat("m", 4096)}
	}
	small := Config{Model: "small"}
	require.NoError(t, Save(big))
	done := make(chan struct{})
	ready := make(chan struct{})
	readerErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(readerErr)
		for first := true; ; first = false {
			cfg, err := Load()
			if first {
				close(ready)
			}
			if err != nil {
				readerErr <- fmt.Errorf("reader could not load config.json: %w", err)
				return
			}
			if !reflect.DeepEqual(cfg, big) && !reflect.DeepEqual(cfg, small) {
				readerErr <- errors.New("reader saw a config.json that matched neither complete config")
				return
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	t.Cleanup(func() {
		close(done)
		wg.Wait()
		require.NoError(t, <-readerErr)
	})
	<-ready
	for n := 0; n < 50; n++ {
		require.NoError(t, Save(big))
		require.NoError(t, Save(small))
	}
	info, err := os.Stat(Path())
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, strings.Contains(e.Name(), ".tmp-"), "leftover temp file %s", e.Name())
	}
}

func TestLoadIgnoresRemovedRenderOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	body := []byte(`{"compact_mode":true,"tool_render":"flat","compact_input":true}`)
	if err := os.WriteFile(filepath.Join(home, "config.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with removed options: %v", err)
	}
	if cfg.CompactInput == nil || !*cfg.CompactInput {
		t.Fatalf("compact_input not preserved: %#v", cfg.CompactInput)
	}
}

// TestConcurrentUpdatesKeepAllIncrements uses real goroutines and file I/O on
// purpose: it checks that the package lock serialises read-modify-write cycles
// against the real config.json.
func TestConcurrentUpdatesKeepAllIncrements(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())

	const workers = 20
	start := make(chan struct{})
	results := make(chan error, workers)
	for range workers {
		go func() {
			<-start
			results <- Update(func(cfg *Config) error {
				cfg.Subagents.MaxConcurrent++
				return nil
			})
		}()
	}
	close(start)
	for range workers {
		if err := <-results; err != nil {
			t.Errorf("Update: %v", err)
		}
	}

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, workers, cfg.Subagents.MaxConcurrent)
}
