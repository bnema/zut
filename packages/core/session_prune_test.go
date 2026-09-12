package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePruneTestSession(t *testing.T, path, cwd string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"meta","meta":{"id":"test-id","cwd":` + strconvQuote(cwd) + `,"model":"m","provider":"p"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func strconvQuote(s string) string {
	quoted := strings.ReplaceAll(s, `\`, `\\`)
	quoted = strings.ReplaceAll(quoted, `"`, `\"`)
	return `"` + quoted + `"`
}

func TestScanStoredSessionGroupsAbsentRoot(t *testing.T) {
	groups, issues := ScanStoredSessionGroups(filepath.Join(t.TempDir(), "no-such-root"))
	if len(groups) != 0 || len(issues) != 0 {
		t.Fatalf("groups=%v issues=%v, want empty", groups, issues)
	}
}

func TestScanStoredSessionGroupsGroupsByCWD(t *testing.T) {
	root := t.TempDir()
	writePruneTestSession(t, filepath.Join(root, "one.jsonl"), "/work/a")
	writePruneTestSession(t, filepath.Join(root, "agents", "named", "two.jsonl"), "/work/a")
	writePruneTestSession(t, filepath.Join(root, "three.jsonl"), "/work/b")

	groups, issues := ScanStoredSessionGroups(root)
	if len(issues) != 0 {
		t.Fatalf("issues=%v, want none", issues)
	}
	if len(groups) != 2 {
		t.Fatalf("groups=%v, want 2", groups)
	}
	if groups[0].CWD != "/work/a" || len(groups[0].Paths) != 2 {
		t.Fatalf("first group=%+v, want /work/a with 2 paths", groups[0])
	}
	if groups[1].CWD != "/work/b" || len(groups[1].Paths) != 1 {
		t.Fatalf("second group=%+v, want /work/b with 1 path", groups[1])
	}
	if groups[0].SizeBytes <= 0 {
		t.Fatalf("group size=%d, want positive", groups[0].SizeBytes)
	}
}

func TestScanStoredSessionGroupsPreservesProblemEntries(t *testing.T) {
	root := t.TempDir()
	writePruneTestSession(t, filepath.Join(root, "ok.jsonl"), "/work/a")
	writePruneTestSession(t, filepath.Join(root, "bad.jsonl"), "")
	if err := os.WriteFile(filepath.Join(root, "broken.jsonl"), []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.jsonl")
	if err := os.Symlink(filepath.Join(root, "ok.jsonl"), link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	groups, issues := ScanStoredSessionGroups(root)
	if len(groups) != 1 || len(groups[0].Paths) != 1 {
		t.Fatalf("groups=%v, want only the valid session", groups)
	}
	if len(issues) != 3 {
		t.Fatalf("issues=%v, want empty-cwd, malformed, and symlink", issues)
	}
	for i := 1; i < len(issues); i++ {
		if issues[i-1].Path > issues[i].Path {
			t.Fatalf("issues unsorted: %v", issues)
		}
	}
}
