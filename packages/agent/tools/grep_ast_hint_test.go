package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepASTHintClassifiesPatterns(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		want    string // substring of the hint; empty means no hint
	}{
		{`fmt\.Errorf\(`, `"fmt.Errorf($$$ARGS)"`},
		{`\bNewClient\(`, `"NewClient($$$ARGS)"`},
		{`os.ReadFile\(`, `"os.ReadFile($$$ARGS)"`},
		{`^func `, "for definitions"},
		{`^\s*def\s`, "for definitions"},
		{`class\b`, "for definitions"},
		{`TODO`, ""},
		{`fmt\.Errorf`, ""},
		{`error: .*\(`, ""},
		{`foo\(bar\)`, ""},
		{`functional`, ""},
	} {
		got := grepASTHint(tc.pattern)
		if tc.want == "" {
			if got != "" {
				t.Errorf("grepASTHint(%q) = %q, want none", tc.pattern, got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("grepASTHint(%q) = %q, want substring %q", tc.pattern, got, tc.want)
		}
	}
}

func TestGrepAppendsASTHintOnlyWhenAvailable(t *testing.T) {
	skipGrepScriptTestsOnWindows(t)
	root := t.TempDir()
	rg := writeGrepScript(t, "#!/bin/sh\nprintf 'main.go:3:\\treturn fmt.Errorf(\"x\")\\n'\n")
	args := json.RawMessage(`{"pattern":"fmt\\.Errorf\\(","path":"."}`)
	for _, tc := range []struct {
		name      string
		available func() bool
		wantHint  bool
	}{
		{"not wired", nil, false},
		{"missing backend", func() bool { return false }, false},
		{"available", func() bool { return true }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := &GrepTool{CWD: root, ASTAvailable: tc.available, LookPath: lookupScripts(map[string]string{"rg": rg})}
			result, err := tool.Execute(context.Background(), args, nil)
			if err != nil {
				t.Fatal(err)
			}
			text := grepText(result)
			if got := strings.Contains(text, "Tip:"); got != tc.wantHint {
				t.Fatalf("hint present = %v, want %v; output %q", got, tc.wantHint, text)
			}
			// The hint must never be read back as a discovered file.
			want := filepath.Join(root, "main.go")
			if len(result.Context.Discovers) != 1 || result.Context.Discovers[0] != want {
				t.Fatalf("discovers = %v, want [%s]", result.Context.Discovers, want)
			}
		})
	}
}
