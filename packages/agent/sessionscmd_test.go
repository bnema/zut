package agent

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestParseSessionSelection(t *testing.T) {
	tests := []struct {
		input   string
		count   int
		want    []int
		wantErr bool
	}{
		{input: "", count: 3},
		{input: "none", count: 3},
		{input: "all", count: 3, want: []int{0, 1, 2}},
		{input: "1,3-4,3", count: 4, want: []int{0, 2, 3}},
		{input: "0", count: 3, wantErr: true},
		{input: "4", count: 3, wantErr: true},
		{input: "3-2", count: 3, wantErr: true},
		{input: "1,,2", count: 3, wantErr: true},
	}
	for _, tt := range tests {
		t.Run("select/"+tt.input, func(t *testing.T) {
			got, err := parseSessionSelection(tt.input, tt.count)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("selection = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseSessionAge(t *testing.T) {
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		input   string
		want    time.Time
		wantErr bool
	}{
		{input: "30m", want: now.Add(-30 * time.Minute)},
		{input: "4h", want: now.Add(-4 * time.Hour)},
		{input: "30d", want: now.Add(-30 * 24 * time.Hour)},
		{input: "2w", want: now.Add(-14 * 24 * time.Hour)},
		{input: "1mo", want: now.AddDate(0, -1, 0)},
		{input: "1y", want: now.AddDate(-1, 0, 0)},
		{input: "", wantErr: true},
		{input: "0d", wantErr: true},
		{input: "-1d", wantErr: true},
		{input: "1.5h", wantErr: true},
		{input: "1s", wantErr: true},
		{input: "99999999999999999999d", wantErr: true},
	}
	for _, tt := range tests {
		t.Run("age/"+tt.input, func(t *testing.T) {
			age, err := parseSessionAge(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseSessionAge(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			got, err := age.cutoff(now)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("cutoff = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSessionAgeClampsCalendarBoundaries(t *testing.T) {
	month, err := parseSessionAge("1mo")
	if err != nil {
		t.Fatal(err)
	}
	got, err := month.cutoff(time.Date(2025, time.March, 31, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2025, time.February, 28, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("one month cutoff = %v, want %v", got, want)
	}

	year, err := parseSessionAge("1y")
	if err != nil {
		t.Fatal(err)
	}
	got, err = year.cutoff(time.Date(2024, time.February, 29, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want = time.Date(2023, time.February, 28, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("one year cutoff = %v, want %v", got, want)
	}
}

func TestParseSessionsPruneOptions(t *testing.T) {
	if _, err := parseSessionsPruneOptions([]string{"--yes"}); err == nil {
		t.Fatal("--yes without --all was accepted")
	}
	if _, err := parseSessionsPruneOptions([]string{"--cwd", "/tmp/project"}); err == nil {
		t.Fatal("--cwd without --older-than was accepted")
	}
	if _, err := parseSessionsPruneOptions([]string{"--all", "--yes", "--dry-run"}); err == nil {
		t.Fatal("--yes with --dry-run was accepted")
	}
	if _, err := parseSessionsPruneOptions([]string{"--bogus"}); err == nil {
		t.Fatal("unknown flag was accepted")
	}
	if _, err := parseSessionsPruneOptions([]string{"--older-than"}); err == nil {
		t.Fatal("missing --older-than value was accepted")
	}
	opts, err := parseSessionsPruneOptions([]string{"--older-than", "1mo", "--cwd", "/tmp/project", "--all", "--yes"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.all || !opts.yes || opts.olderThan == nil || opts.olderThan.unit != "mo" || opts.cwd != "/tmp/project" {
		t.Fatalf("options = %#v", opts)
	}
}

func TestSessionsPruneDryRunPreservesSessions(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing-project")
	existing := t.TempDir()
	missingPath := createPruneTestSession(t, root, missing)
	existingPath := createPruneTestSession(t, root, existing)

	var out, errOut bytes.Buffer
	if err := runSessionsPrune(root, sessionsPruneOptions{dryRun: true}, strings.NewReader(""), &out, &errOut, os.Stat); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), missing) || strings.Contains(out.String(), existing) {
		t.Fatalf("output = %q, want only missing cwd", out.String())
	}
	if !strings.Contains(out.String(), "dry run: 1 session in 1 directory would be deleted") {
		t.Fatalf("output = %q, want dry-run summary", out.String())
	}
	for _, path := range []string{missingPath, existingPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("dry run removed %s: %v", path, err)
		}
	}
}

func TestSessionsPruneInteractiveDeletesSelectedGroup(t *testing.T) {
	root := t.TempDir()
	firstCWD := filepath.Join(root, "gone-a")
	secondCWD := filepath.Join(root, "gone-b")
	first := createPruneTestSession(t, root, firstCWD)
	second := createPruneTestSession(t, root, secondCWD)

	var out, errOut bytes.Buffer
	if err := runSessionsPrune(root, sessionsPruneOptions{}, strings.NewReader("2\ny\n"), &out, &errOut, os.Stat); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("unselected session was removed: %v", err)
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatalf("selected session still exists or stat failed unexpectedly: %v", err)
	}
	if !strings.Contains(out.String(), "deleted 1 session from 1 directory") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSessionsPruneDeclinedConfirmationDeletesNothing(t *testing.T) {
	root := t.TempDir()
	path := createPruneTestSession(t, root, filepath.Join(root, "gone"))

	var out, errOut bytes.Buffer
	if err := runSessionsPrune(root, sessionsPruneOptions{all: true}, strings.NewReader("n\n"), &out, &errOut, os.Stat); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("declined confirmation removed the session: %v", err)
	}
	if !strings.Contains(out.String(), "no sessions deleted") {
		t.Fatalf("output = %q, want declined message", out.String())
	}
}

func TestSessionsPruneAllYesDeletesWithoutPrompting(t *testing.T) {
	root := t.TempDir()
	first := createPruneTestSession(t, root, filepath.Join(root, "gone-a"))
	second := createPruneTestSession(t, root, filepath.Join(root, "gone-b"))

	var out, errOut bytes.Buffer
	if err := runSessionsPrune(root, sessionsPruneOptions{all: true, yes: true}, strings.NewReader(""), &out, &errOut, os.Stat); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{first, second} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("session %s still exists: %v", path, err)
		}
	}
	if !strings.Contains(out.String(), "deleted 2 sessions from 2 directories") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSessionsPrunePreservesGroupWhenDirectoryCheckIsInconclusive(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "unavailable-mount")
	path := createPruneTestSession(t, root, cwd)
	stat := func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission }

	var out, errOut bytes.Buffer
	if err := runSessionsPrune(root, sessionsPruneOptions{all: true, yes: true}, strings.NewReader(""), &out, &errOut, stat); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("session was removed after inconclusive stat: %v", err)
	}
	if !strings.Contains(errOut.String(), "permission denied") {
		t.Fatalf("stderr = %q, want stat warning", errOut.String())
	}
	if !strings.Contains(out.String(), "no stale session directories found") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSessionsPruneRechecksDirectoryBeforeDeleting(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "temporarily-missing")
	path := createPruneTestSession(t, root, cwd)
	calls := 0
	stat := func(string) (fs.FileInfo, error) {
		calls++
		if calls == 1 {
			return nil, fs.ErrNotExist
		}
		return nil, nil
	}

	var out, errOut bytes.Buffer
	if err := runSessionsPrune(root, sessionsPruneOptions{all: true, yes: true}, strings.NewReader(""), &out, &errOut, stat); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("session was removed after cwd reappeared: %v", err)
	}
	if !strings.Contains(errOut.String(), "directory now exists") {
		t.Fatalf("stderr = %q, want recheck warning", errOut.String())
	}
}

func TestSessionsPruneOlderThanFiltersByLastActivity(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	oldPath := createPruneTestSession(t, root, cwd)
	newPath := createPruneTestSession(t, root, cwd)
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	age, err := parseSessionAge("24h")
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	opts := sessionsPruneOptions{dryRun: true, olderThan: &age}
	if err := runSessionsPrune(root, opts, strings.NewReader(""), &out, &errOut, os.Stat); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "dry run: 1 session in 1 directory would be deleted") {
		t.Fatalf("output = %q", out.String())
	}
	for _, path := range []string{oldPath, newPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("dry run removed %s: %v", path, err)
		}
	}
}

func TestSessionsPruneOlderThanLimitsDeletionToCWD(t *testing.T) {
	root := t.TempDir()
	firstCWD := t.TempDir()
	secondCWD := t.TempDir()
	first := createPruneTestSession(t, root, firstCWD)
	second := createPruneTestSession(t, root, secondCWD)
	oldTime := time.Now().Add(-48 * time.Hour)
	for _, path := range []string{first, second} {
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}

	age, err := parseSessionAge("24h")
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	opts := sessionsPruneOptions{all: true, yes: true, olderThan: &age, cwd: firstCWD}
	if err := runSessionsPrune(root, opts, strings.NewReader(""), &out, &errOut, os.Stat); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("matching session still exists or stat failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("session from another cwd was removed: %v", err)
	}
}

func TestSessionsPruneWarnsOnScanIssues(t *testing.T) {
	root := t.TempDir()
	kept := createPruneTestSession(t, root, filepath.Join(root, "gone"))
	if err := os.WriteFile(filepath.Join(root, "broken.jsonl"), []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if err := runSessionsPrune(root, sessionsPruneOptions{all: true, yes: true}, strings.NewReader(""), &out, &errOut, os.Stat); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "warning: preserving") {
		t.Fatalf("stderr = %q, want scan-issue warning", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(root, "broken.jsonl")); err != nil {
		t.Fatalf("malformed entry was removed: %v", err)
	}
	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Fatalf("stale session still exists: %v", err)
	}
}

func TestSessionsCommandDispatch(t *testing.T) {
	if handled, err := runSessionsCommand(nil); handled || err != nil {
		t.Fatalf("handled=%v err=%v, want passthrough", handled, err)
	}
	if handled, err := runSessionsCommand([]string{"other"}); handled || err != nil {
		t.Fatalf("handled=%v err=%v, want passthrough", handled, err)
	}
	if handled, err := runSessionsCommand([]string{"sessions", "bogus"}); !handled || err == nil {
		t.Fatalf("handled=%v err=%v, want handled error", handled, err)
	}
}

func createPruneTestSession(t *testing.T, root, cwd string) string {
	t.Helper()
	session, err := core.NewSession(root, cwd, "test", "test-model", "test-version")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	return session.Path
}
