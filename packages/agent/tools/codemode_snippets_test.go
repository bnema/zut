package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func snippetTestStore(t *testing.T) *codemodeSnippetStore {
	t.Helper()
	t.Setenv("ZUT_HOME", t.TempDir())
	return &codemodeSnippetStore{home: func() string { return os.Getenv("ZUT_HOME") }}
}

func TestCodemodeSnippetStore(t *testing.T) {
	s := snippetTestStore(t)
	if entries, err := s.list(); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	if err := s.save("sample", "result = 1\n", "example"); err != nil {
		t.Fatal(err)
	}
	code, err := s.read("sample")
	if err != nil || code != "result = 1\n" {
		t.Fatal(code, err)
	}
	entries, err := s.list()
	if err != nil || len(entries) != 1 || entries[0].Description != "example" || entries[0].UpdatedAt == "" {
		t.Fatal(entries, err)
	}
	created := entries[0].CreatedAt
	if err := s.save("sample", "result = 2", "updated"); err != nil {
		t.Fatal(err)
	}
	entries, _ = s.list()
	if entries[0].CreatedAt != created || entries[0].Description != "updated" {
		t.Fatal(entries)
	}
	for _, name := range []string{"", "a/b", `a\b`, "../outside", strings.Repeat("a", 65)} {
		if err := s.save(name, "", ""); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if err := s.save("large", strings.Repeat("x", codemodeSnippetMax+1), ""); err == nil {
		t.Fatal("accepted oversized")
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{"codemode": 0700, "codemode/snippets": 0700, "codemode/snippets/sample.py": 0600} {
			stat, err := os.Stat(filepath.Join(s.home(), path))
			if err != nil || stat.Mode().Perm() != want {
				t.Fatal(path, stat, err)
			}
		}
	}
	if err := s.delete("sample"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.read("sample"); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestCodemodeSnippetSymlinksAndAdmission(t *testing.T) {
	for _, component := range []string{"codemode", "codemode/snippets", "codemode/snippets/escape.py"} {
		t.Run(component, func(t *testing.T) {
			s := snippetTestStore(t)
			if err := os.MkdirAll(filepath.Dir(filepath.Join(s.home(), component)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(s.home(), component)); err != nil {
				t.Skip(err)
			}
			if err := s.save("escape", "result=1", ""); err == nil {
				t.Fatal("followed symlink")
			}
		})
	}
	s := snippetTestStore(t)
	s.sandbox = NewSandbox(t.TempDir())
	s.sandbox.Lock()
	if err := s.save("denied", "", ""); err == nil {
		t.Fatal("jailed write")
	}
	s.sandbox.Unlock()
	s.sandbox.Permissions = &PermissionSet{}
	if err := s.save("denied", "", ""); err == nil {
		t.Fatal("restricted write")
	}
}

func TestCodemodeSnippetBridgeAndSearch(t *testing.T) {
	s := snippetTestStore(t)
	b := newCodemodeBridge(&bridgeTestInvoker{}, "result = 42")
	b.snippets = s
	reply := b.handle(context.Background(), []byte(`{"method":"save_snippet","params":{"name":"answer","description":"saved docs"}}`))
	if reply.Error != nil {
		t.Fatal(reply.Error)
	}
	if code, err := s.read("answer"); err != nil || code != b.code {
		t.Fatal(code, err)
	}
	reply = b.handle(context.Background(), []byte(`{"method":"search","params":{"query":"answer docs","limit":1}}`))
	raw, _ := json.Marshal(reply.Result)
	if !strings.Contains(string(raw), `"kind":"snippet"`) || !strings.Contains(string(raw), `"total":1`) {
		t.Fatal(string(raw), reply.Error)
	}
}

func TestCodemodeRealPythonSnippets(t *testing.T) {
	s := snippetTestStore(t)
	tool := &CodemodeTool{CWD: t.TempDir(), Env: os.Environ(), Interpreter: codemodePython(t), SnippetHome: s.home}
	code := `zut.save_snippet("self")
assert "zut.save_snippet" in zut.snippet("self")
zut.save_snippet("compose", 'result = zut.call("echo_data", **inputs).data', description="echo composition")
assert zut.run_snippet("compose", x=3) == {"x": 3}
zut.save_snippet("fresh", 'result = "result" in globals()')
assert zut.run_snippet("fresh") is False
assert zut.run_snippet("fresh") is False
assert [s["name"] for s in zut.snippets()] == ["compose", "fresh", "self"]
s = zut.search("compose", limit=1)
assert s.total == 1 and s[0]["kind"] == "snippet"
assert zut.search("echo_data")[0]["kind"] == "tool"
zut.delete_snippet("compose")
assert not zut.search("compose")
zut.result("snippets ok")`
	result := runCodemodeAgent(t, tool, code, 10, nil, &codemodeEchoDataTool{})
	if result.IsError || !strings.Contains(codemodeOutput(t, result), "snippets ok") {
		t.Fatal(codemodeOutput(t, result))
	}
}

func TestCodemodeSnippetLimitsAndLiveDenial(t *testing.T) {
	s := snippetTestStore(t)
	if err := s.save("max", strings.Repeat("x", codemodeSnippetMax), ""); err != nil {
		t.Fatal(err)
	}
	if code, err := s.read("max"); err != nil || len(code) != codemodeSnippetMax {
		t.Fatal(len(code), err)
	}
	if err := s.save("desc", "", strings.Repeat("x", 1025)); err == nil {
		t.Fatal("oversized description")
	}
	s.sandbox = NewSandbox(t.TempDir())
	b := newCodemodeBridge(&bridgeTestInvoker{}, "")
	b.snippets = s
	s.sandbox.Lock()
	for _, method := range []string{"save_snippet", "snippet", "snippets", "delete_snippet"} {
		raw, _ := json.Marshal(map[string]any{"method": method, "params": map[string]string{"name": "max"}})
		if reply := b.handle(context.Background(), raw); reply.Error == nil || reply.Error.Kind != "not_allowed" {
			t.Fatalf("%s: %#v", method, reply)
		}
	}
}
