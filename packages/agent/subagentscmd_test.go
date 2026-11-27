package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/internal/flock"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/google/uuid"
)

func TestSubagentsPruneOptions(t *testing.T) {
	opts, err := parseSubagentsPruneOptions([]string{"--older-than", "8d", "-y"})
	if err != nil || !opts.yes || opts.age != (sessionAge{8, "d"}) {
		t.Fatalf("options = %#v, %v", opts, err)
	}
	for _, args := range [][]string{{"--dry-run", "--yes"}, {"--older-than"}, {"--older-than", "0d"}, {"--unknown"}} {
		if _, err := parseSubagentsPruneOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestSubagentsPrune(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "subagents")
	var fixtures []struct{ name, id, path string }
	for _, kind := range []string{"eligible", "busy"} {
		id := writeCLIPruneChild(t, root, "runtime:"+uuid.NewString())
		dir := filepath.Join(root, id)
		if kind == "busy" {
			holdPruneTestLock(t, filepath.Join(dir, "owner.lock"))
		}
		fixtures = append(fixtures, struct{ name, id, path string }{kind, id, dir})
	}
	if err := os.Mkdir(filepath.Join(root, ".prune-leftover"), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(args []string, answer string) (string, string, error) {
		t.Helper()
		opts, err := parseSubagentsPruneOptions(args)
		if err != nil {
			t.Fatal(err)
		}
		var out, stderr bytes.Buffer
		err = runSubagentsPrune(t.Context(), home, opts, strings.NewReader(answer), &out, &stderr)
		return out.String(), stderr.String(), err
	}
	assertPresent := func(deleted bool) {
		t.Helper()
		for _, f := range fixtures {
			_, err := os.Lstat(f.path)
			if f.name == "eligible" && deleted {
				if !os.IsNotExist(err) {
					t.Errorf("eligible not deleted: %v", err)
				}
			} else if err != nil {
				t.Errorf("%s not preserved: %v", f.name, err)
			}
		}
	}
	before := pruneFixtureSnapshot(t, home)
	out, stderr, err := run([]string{"--dry-run"}, "")
	if err != nil || !strings.Contains(out, fixtures[0].id[:8]) || !strings.Contains(out, "2 candidate(s)") || !strings.Contains(stderr, ".prune-leftover") {
		t.Fatalf("dry run: %q / %q / %v", out, stderr, err)
	}
	if after := pruneFixtureSnapshot(t, home); !reflect.DeepEqual(before, after) {
		t.Fatal("dry run changed paths, contents, modes or mtimes")
	}

	assertPresent(false)
	out, stderr, err = run(nil, "n\n")
	if err != nil || !strings.Contains(out, "no resident children deleted") || !strings.Contains(stderr, subagentsOfflineWarning) {
		t.Fatalf("decline: %q / %v", out, err)
	}
	assertPresent(false)
	for _, answer := range []string{"", "y", "yes"} {
		out, stderr, err = run(nil, answer)
		if err != nil || !strings.Contains(out, "no resident children deleted") || !strings.Contains(stderr, "[y/N]") {
			t.Fatalf("unterminated answer %q: %q / %q / %v", answer, out, stderr, err)
		}
		assertPresent(false)
	}
	for _, stream := range []string{"preview", "prompt"} {
		t.Run(stream+" write failure", func(t *testing.T) {
			var output, warnings bytes.Buffer
			var out, errOut io.Writer = &output, &warnings
			broken, err := os.CreateTemp(t.TempDir(), "closed")
			if err != nil {
				t.Fatal(err)
			}
			if err := broken.Close(); err != nil {
				t.Fatal(err)
			}
			if stream == "preview" {
				out = broken
			} else {
				errOut = io.MultiWriter(&warnings, failPrunePromptWriter{out: broken})
			}
			opts, _ := parseSubagentsPruneOptions(nil)
			if err := runSubagentsPrune(t.Context(), home, opts, strings.NewReader("yes\n"), out, errOut); err == nil {
				t.Fatal("expected output error")
			}
			assertPresent(false)
		})
	}
	t.Run("interrupt confirmation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		input, inputWriter := io.Pipe()
		defer input.Close()
		defer inputWriter.Close()
		var output bytes.Buffer
		opts, _ := parseSubagentsPruneOptions(nil)
		err := runSubagentsPrune(ctx, home, opts, input, &output, cancelPrunePrompt{cancel})
		if !errors.Is(err, context.Canceled) || !strings.Contains(output.String(), "deleted 0 resident child(ren) before interruption") {
			t.Fatalf("interrupt: %q / %v", output.String(), err)
		}
		assertPresent(false)
	})
	out, stderr, err = run([]string{"--yes"}, "")
	if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(32)) && strings.Contains(stderr, "cannot stage while holding lease") {
		assertPresent(false)
		return
	}
	if err != nil || !strings.Contains(out, "deleted 1 resident") || !strings.Contains(stderr, "in use") || !strings.Contains(stderr, subagentsOfflineWarning) {
		t.Fatalf("yes: %q / %q / %v", out, stderr, err)
	}
	assertPresent(true)
}

func TestSubagentsPruneHostBusy(t *testing.T) {
	home := t.TempDir()
	holdPruneTestLock(t, filepath.Join(home, "daemon", "daemon.lock"))
	opts, _ := parseSubagentsPruneOptions([]string{"--yes"})
	var out, warnings bytes.Buffer
	err := runSubagentsPrune(t.Context(), home, opts, strings.NewReader(""), &out, &warnings)
	if err == nil || !strings.Contains(err.Error(), "requires an offline ZUT_HOME") {
		t.Fatalf("error = %v", err)
	}
	opts, _ = parseSubagentsPruneOptions([]string{"--dry-run"})
	if err := runSubagentsPrune(t.Context(), home, opts, strings.NewReader(""), &out, &warnings); err != nil {
		t.Fatal(err)
	}
}

func holdPruneTestLock(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSubagentsPruneLeaseHelper$")
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(), "ZUT_PRUNE_TEST_LOCK="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("lease helper: %q %v", line, err)
	}
}

func TestSubagentsPruneLeaseHelper(t *testing.T) {
	path := os.Getenv("ZUT_PRUNE_TEST_LOCK")
	if path == "" {
		return
	}
	lease, err := flock.TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	_, _ = os.Stdout.WriteString("ready\n")
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
}

func pruneFixtureSnapshot(t *testing.T, home string) map[string]struct {
	mode    os.FileMode
	mtime   time.Time
	content string
} {
	t.Helper()
	type snapshot = struct {
		mode    os.FileMode
		mtime   time.Time
		content string
	}
	result := map[string]snapshot{}
	err := filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		item := snapshot{mode: info.Mode(), mtime: info.ModTime()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			item.content = string(data)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			item.content, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		result[path] = item
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSubagentsPruneRejectsSymlinkedDaemonLock(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "untouched")
	if err := os.WriteFile(target, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "daemon")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "daemon.lock")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	before := pruneFixtureSnapshot(t, home)
	opts, _ := parseSubagentsPruneOptions([]string{"--yes"})
	var out, warnings bytes.Buffer
	err := runSubagentsPrune(t.Context(), home, opts, strings.NewReader(""), &out, &warnings)
	if err == nil || !strings.Contains(err.Error(), "daemon.lock is not a regular file") {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(before, pruneFixtureSnapshot(t, home)) {
		t.Fatal("invalid lock changed the fixture")
	}
}

// Allow warnings to be written but fail the confirmation write itself.
type failPrunePromptWriter struct{ out io.Writer }

func (w failPrunePromptWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("[y/N]")) {
		return w.out.Write(p)
	}
	return len(p), nil
}

func TestSubagentsHelpRejectsExtraArguments(t *testing.T) {
	handled, err := runSubagentsCommand(t.Context(), []string{"subagents", "help", "--typo"})
	if !handled || err == nil {
		t.Fatalf("handled = %t, err = %v", handled, err)
	}
}

// Cancel only once the CLI writes the confirmation prompt. The input pipe
// never supplies an answer, so cancellation must unblock the command itself.
type cancelPrunePrompt struct{ cancel context.CancelFunc }

func (w cancelPrunePrompt) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("[y/N]")) {
		w.cancel()
	}
	return len(p), nil
}

func writeCLIPruneChild(t *testing.T, root, parent string) string {
	t.Helper()
	id := uuid.NewString()
	j, err := subagents.OpenResidentJournal(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-8 * 24 * time.Hour).UTC()
	spec := subagents.ResidentChildSpec{ID: id, SessionID: id, ParentSessionID: parent, InitialTurnID: "turn", Provider: "test", Model: "test"}
	var rows bytes.Buffer
	enc := json.NewEncoder(&rows)
	for _, r := range []map[string]any{{"version": 3, "type": "child.accepted", "time": at, "spec": spec}, {"version": 3, "type": "turn.started", "time": at, "turn_id": "turn"}, {"version": 3, "type": "turn.finished", "time": at, "turn_id": "turn", "outcome": "completed"}} {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	metadata, err := json.Marshal(subagents.ResidentMetadata{Version: 3, ID: id, SessionID: id, ParentSessionID: parent, State: subagents.ResidentIdle, UpdatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"metadata.json": metadata, "transcript.jsonl": rows.Bytes()} {
		if err := os.WriteFile(filepath.Join(j.Dir(), name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestSubagentsPruneReportsCountOnSecondChildCancellation(t *testing.T) {
	root := t.TempDir()
	for range 2 {
		writeCLIPruneChild(t, root, "runtime:"+uuid.NewString())
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	checks := 0
	pruner := subagents.ResidentPruner{Root: root, Cutoff: time.Now().Add(-7 * 24 * time.Hour), RecheckParentActivity: func(string) (time.Time, error) {
		checks++
		if checks == 2 {
			cancel()
		}
		return time.Time{}, nil
	}}
	candidates, notices, err := pruner.Scan()
	if err != nil || len(notices) != 0 || len(candidates) != 2 {
		t.Fatalf("scan: %v / %v / %v", candidates, notices, err)
	}
	var output, warnings bytes.Buffer
	deleted, err := deletePruneCandidates(ctx, pruner, candidates, &warnings)
	if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(32)) && strings.Contains(warnings.String(), "cannot stage while holding lease") {
		t.Skip("Windows sharing violation during staging")
	}
	if err != nil {
		t.Fatal(err)
	}
	err = reportPruneInterruption(&output, deleted, ctx.Err())
	if !errors.Is(err, context.Canceled) || output.String() != "deleted 1 resident child(ren) before interruption\n" {
		t.Fatalf("report: %q / %v", output.String(), err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != candidates[1].ID {
		t.Fatalf("remaining: %v / %v", entries, err)
	}
}

// Replace the parent transcript once the CLI asks for confirmation, after the
// scan resolved the parent and before the fresh recheck that guards staging.
type replaceParentOnPrompt struct{ replace func() }

func (w replaceParentOnPrompt) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("[y/N]")) {
		w.replace()
	}
	return len(p), nil
}

func TestSubagentsPruneRechecksParentThroughManagedStore(t *testing.T) {
	old := time.Now().Add(-8 * 24 * time.Hour)
	writeOldSession := func(t *testing.T, path string, meta core.SessionMeta) {
		t.Helper()
		line, err := json.Marshal(map[string]any{"type": "meta", "meta": meta})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name string
		// replace rewrites the parent transcript; nil leaves it untouched.
		replace     func(t *testing.T, path string, meta core.SessionMeta)
		wantDeleted bool
	}{
		{name: "unchanged parent", wantDeleted: true},
		{name: "header with different ID", replace: func(t *testing.T, path string, meta core.SessionMeta) {
			meta.ID = uuid.NewString()
			tmp := path + ".new"
			writeOldSession(t, tmp, meta)
			if err := os.Rename(tmp, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "same ID in a new file", replace: func(t *testing.T, path string, meta core.SessionMeta) {
			tmp := path + ".new"
			writeOldSession(t, tmp, meta)
			if err := os.Rename(tmp, path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			session, err := core.NewSession(home, t.TempDir(), "test", "test-model", "test-version")
			if err != nil {
				t.Fatal(err)
			}
			if err := session.AppendMessage(provider.Message{
				Role:    provider.RoleUser,
				Content: []provider.Content{provider.TextBlock{Text: "hello"}},
			}); err != nil {
				t.Fatal(err)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			parent := session.Path
			if err := os.Chtimes(parent, old, old); err != nil {
				t.Fatal(err)
			}
			meta, err := core.ManagedSessionMeta(t.Context(), home, parent)
			if err != nil || meta.ID != session.ID {
				t.Fatalf("parent meta = %+v, %v", meta, err)
			}
			if found, err := core.FindManagedSessionByID(t.Context(), home, meta.ID); err != nil || found != parent {
				t.Fatalf("parent lookup = %q, %v; want %q", found, err, parent)
			}
			child := writeCLIPruneChild(t, filepath.Join(home, "subagents"), meta.ID)
			childDir := filepath.Join(home, "subagents", child)

			dryRun, _ := parseSubagentsPruneOptions([]string{"--dry-run"})
			var out, warnings bytes.Buffer
			if err := runSubagentsPrune(t.Context(), home, dryRun, strings.NewReader(""), &out, &warnings); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "1 candidate(s)") || !strings.Contains(out.String(), child[:8]) {
				t.Fatalf("old child with old parent is not eligible: %q / %q", out.String(), warnings.String())
			}

			opts, _ := parseSubagentsPruneOptions(nil)
			out.Reset()
			warnings.Reset()
			prompt := replaceParentOnPrompt{replace: func() {
				if tt.replace != nil {
					tt.replace(t, parent, meta)
				}
			}}
			err = runSubagentsPrune(t.Context(), home, opts, strings.NewReader("y\n"), &out, io.MultiWriter(&warnings, prompt))
			if err != nil {
				t.Fatalf("prune: %v / %q / %q", err, out.String(), warnings.String())
			}
			_, statErr := os.Lstat(childDir)
			if tt.wantDeleted {
				if !os.IsNotExist(statErr) || !strings.Contains(out.String(), "deleted 1 resident") {
					t.Fatalf("child not deleted: %v / %q", statErr, out.String())
				}
				return
			}
			if statErr != nil {
				t.Fatalf("child with replaced parent was deleted: %v", statErr)
			}
			if !strings.Contains(out.String(), "deleted 0 resident") || !strings.Contains(warnings.String(), "unresolved parent") {
				t.Fatalf("replaced parent: %q / %q", out.String(), warnings.String())
			}
		})
	}
}
