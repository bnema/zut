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
	for _, size := range []int64{maxLogSize, maxLogSize + 1} {
		t.Run(string(rune('a'+size-maxLogSize)), func(t *testing.T) {
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
	log, closer, err := OpenLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
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
}
