package scheduler

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestSchedulePersistence(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	store := FileStore{Path: filepath.Join(t.TempDir(), "daemon", "schedules.json")}
	e, err := NewPersistentEngine(func() time.Time { return now }, store)
	if err != nil {
		t.Fatal(err)
	}
	task, err := e.Add(NewTaskInput{SessionID: "a", Cron: "* * * * *", Message: "check", Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	e.finish(task.ID, task.NextRun, errors.New("test failure"))
	now = now.Add(24 * time.Hour)
	restored, err := NewPersistentEngine(func() time.Time { return now }, store)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := restored.Get(task.ID)
	if !ok || !got.NextRun.Equal(now.Add(time.Minute)) || got.RunCount != 1 || got.LastError != "test failure" || !got.LastRun.Equal(task.NextRun) || got.queued || got.running {
		t.Fatalf("restored: %+v", got)
	}
	info, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("mode: %v", info.Mode())
	}
	if !restored.Cancel(task.ID) {
		t.Fatal("Cancel failed")
	}
	reloaded, err := NewPersistentEngine(nil, store)
	if err != nil || len(reloaded.List()) != 0 {
		t.Fatalf("cancel reload: %v, %v", reloaded, err)
	}
}

func TestScheduleCancelSessionConcurrent(t *testing.T) {
	store := FileStore{Path: filepath.Join(t.TempDir(), "schedules.json")}
	e, err := NewPersistentEngine(nil, store)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task, err := e.Add(NewTaskInput{SessionID: "a", Cron: "@hourly", Message: "check", Location: time.UTC})
			if err != nil {
				t.Error(err)
				return
			}
			if !e.Cancel(task.ID) {
				t.Error("Cancel failed")
			}
		}()
	}
	wg.Wait()
	for _, id := range []string{"a", "a", "b"} {
		if _, err := e.Add(NewTaskInput{SessionID: id, Cron: "@hourly", Message: "check", Location: time.UTC}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.CancelSession("a"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewPersistentEngine(nil, store)
	if err != nil {
		t.Fatal(err)
	}
	tasks := reloaded.List()
	if len(tasks) != 1 || tasks[0].SessionID != "b" {
		t.Fatalf("tasks: %+v", tasks)
	}
}

func TestScheduleStoreErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.json")
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPersistentEngine(nil, FileStore{Path: path}); err == nil {
		t.Fatal("accepted malformed JSON")
	}
	e, err := NewPersistentEngine(nil, FileStore{Path: filepath.Join(dir, "valid.json")})
	if err != nil {
		t.Fatal(err)
	}
	task, err := e.Add(NewTaskInput{SessionID: "a", Cron: "@hourly", Message: "check", Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	// Replacing the destination with a directory forces a real atomic-save error.
	if err := os.Remove(filepath.Join(dir, "valid.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "valid.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if e.Cancel(task.ID) || e.Err() == nil {
		t.Fatal("cancel hid save failure")
	}
	if _, ok := e.Get(task.ID); !ok {
		t.Fatal("failed cancel removed task")
	}
	if err := e.CancelSession("a"); err == nil {
		t.Fatal("CancelSession hid failure")
	}
	if _, err := e.Add(NewTaskInput{SessionID: "a", Cron: "@hourly", Message: "check", Location: time.UTC}); err == nil {
		t.Fatal("Add hid failure")
	}
	if len(e.List()) != 1 {
		t.Fatal("failed Add retained task")
	}
}

func TestScheduleRestoresTimezone(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	for _, name := range []string{"America/New_York", "custom"} {
		t.Run(name, func(t *testing.T) {
			loc := time.FixedZone(name, 2*60*60)
			if name != "custom" {
				var err error
				loc, err = time.LoadLocation(name)
				if err != nil {
					t.Fatal(err)
				}
			}
			store := FileStore{Path: filepath.Join(t.TempDir(), "schedules.json")}
			e, err := NewPersistentEngine(func() time.Time { return now }, store)
			if err != nil {
				t.Fatal(err)
			}
			task, err := e.Add(NewTaskInput{SessionID: "a", Cron: "0 9 * * *", Message: "check", Location: loc})
			if err != nil {
				t.Fatal(err)
			}
			restored, err := NewPersistentEngine(func() time.Time { return now }, store)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := restored.Get(task.ID)
			if !ok || !got.NextRun.Equal(task.NextRun) {
				t.Fatalf("restored timezone deadline: %v, want %v", got.NextRun, task.NextRun)
			}
		})
	}
}
