package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestOpenLogLevels(t *testing.T) {
	for _, tc := range []struct {
		value string
		level slog.Level
	}{
		{"", slog.LevelInfo}, {"info", slog.LevelInfo}, {"debug", slog.LevelDebug},
		{"warn", slog.LevelWarn}, {"error", slog.LevelError}, {"invalid", slog.LevelInfo},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("ZUT_DAEMON_LOG", tc.value)
			var buf bytes.Buffer
			log, closer, err := OpenLog(t.TempDir(), &buf)
			if err != nil {
				t.Fatal(err)
			}
			defer closer.Close()
			for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
				if got := log.Enabled(context.Background(), level); got != (level >= tc.level) {
					t.Errorf("enabled %v = %v", level, got)
				}
				log.Log(context.Background(), level, level.String())
				if got := strings.Contains(buf.String(), "msg="+level.String()); got != (level >= tc.level) {
					t.Errorf("output for %v = %v", level, got)
				}
			}
		})
	}
}

func TestOpenLogRotation(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
	}{{"at_limit", maxLogSize}, {"over_limit", maxLogSize + 1}} {
		size := tc.size
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := LogPath(home)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+".1", []byte("obsolete"), 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(size); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			log, closer, err := OpenLog(home)
			if err != nil {
				t.Fatal(err)
			}
			defer closer.Close()
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			want := size
			if size > maxLogSize {
				want = 0
			}
			if st.Size() != want {
				t.Fatalf("startup size = %d, want %d", st.Size(), want)
			}
			log.Info("after rotation")
			backup, err := os.Stat(path + ".1")
			if err != nil {
				t.Fatal(err)
			}
			if backup.Size() != size {
				t.Fatalf("backup size = %d, want %d", backup.Size(), size)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "after rotation") {
				t.Fatal("missing new record")
			}
			for _, name := range []string{path, path + ".1"} {
				st, err := os.Stat(name)
				if err != nil {
					t.Fatal(err)
				}
				if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
					t.Errorf("%s permissions = %o", name, st.Mode().Perm())
				}
			}
		})
	}
}

func TestLogConcurrentRotation(t *testing.T) {
	home := t.TempDir()
	log, closer, err := OpenLog(home)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 40 {
				log.Info("record", "kind", strings.Repeat("x", 100000))
			}
		}()
	}
	wg.Wait()
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	path := LogPath(home)
	for _, name := range []string{path, path + ".1"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if int64(len(data)) > maxLogSize {
			t.Errorf("%s size %d > %d", name, len(data), maxLogSize)
		}
		if len(data) == 0 || data[len(data)-1] != '\n' {
			t.Fatalf("%s does not end with a complete line", name)
		}
		for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
			if !strings.HasPrefix(line, "time=") || !strings.HasSuffix(line, "kind="+strings.Repeat("x", 100000)) {
				t.Fatalf("%s line %d incomplete (len %d)", name, i, len(line))
			}
		}
	}
}

func TestLogKeepsWritingWhenRotationFails(t *testing.T) {
	home := t.TempDir()
	path := LogPath(home)
	if err := os.MkdirAll(path+".1", 0o700); err != nil {
		t.Fatal(err)
	}
	// A non-empty directory cannot be removed or replaced by the rename.
	if err := os.WriteFile(filepath.Join(path+".1", "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	log, closer, err := OpenLog(home, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	big := strings.Repeat("x", 1<<20)
	for range 11 {
		log.Info("fill", "kind", big)
	}
	log.Info("after failed rotation")
	if w := closer.(*rotatingLog); w.rotateErr == nil {
		t.Fatal("rotation should have failed")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) <= maxLogSize || !strings.Contains(string(data), "after failed rotation") {
		t.Fatalf("log stopped after failed rotation (size %d)", len(data))
	}
	if !strings.Contains(stderr.String(), "after failed rotation") {
		t.Fatal("stderr went quiet")
	}
}
