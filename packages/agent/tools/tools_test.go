package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReadText(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &ReadTool{CWD: dir}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": "a.txt"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Content[0].(provider.TextBlock).Text
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Fatalf("got %q", got)
	}
}

func TestReadImageMimeFromContentNotExtension(t *testing.T) {
	// A file named .png whose bytes are actually JPEG. The MIME must be
	// sniffed from the content (image/jpeg), not the extension, or
	// providers that validate the declared media type reject the request.
	dir := t.TempDir()
	p := filepath.Join(dir, "shot.png")
	jpegBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}
	if err := os.WriteFile(p, jpegBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &ReadTool{CWD: dir}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": "shot.png"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	img, ok := res.Content[0].(provider.ImageBlock)
	if !ok {
		t.Fatalf("expected ImageBlock, got %T", res.Content[0])
	}
	if img.MimeType != "image/jpeg" {
		t.Fatalf("mime from extension not corrected: got %s want image/jpeg", img.MimeType)
	}
}

func TestReadOffsetLimit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("1\n2\n3\n4\n5\n"), 0o644)
	tool := &ReadTool{CWD: dir}
	res, _ := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": "a.txt", "offset": 2, "limit": 2}), nil)
	got := res.Content[0].(provider.TextBlock).Text
	// Current output format is raw bytes (no embedded line numbers):
	// the tui draws its own gutter from the `start_line` detail.
	if got != "2\n3\n" {
		t.Fatalf("want \"2\\n3\\n\", got %q", got)
	}
	if start, ok := res.Details.(map[string]any)["start_line"]; !ok || start != 2 {
		t.Errorf("start_line detail want 2, got %v", start)
	}
}

func TestReadBinaryRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "b.bin")
	os.WriteFile(p, []byte{0x00, 0x01, 0x02}, 0o644)
	tool := &ReadTool{CWD: dir}
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": "b.bin"}), nil); err == nil {
		t.Fatal("want binary rejection")
	}
}

func TestWriteCreatesDirs(t *testing.T) {
	dir := t.TempDir()
	tool := &WriteTool{CWD: dir}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": "sub/a.txt", "content": "hi"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "sub", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hi" {
		t.Fatalf("got %q", string(b))
	}
}

func TestEditSingle(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("hello world\n"), 0o644)
	tool := &EditTool{CWD: dir}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"path":  "a.txt",
		"edits": []map[string]any{{"oldText": "world", "newText": "gopher"}},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "hello gopher\n" {
		t.Fatalf("got %q", string(b))
	}
}

func TestEditPreviewReturnsDiffWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &EditTool{CWD: dir}
	args := mustJSON(t, map[string]any{
		"path":  "a.txt",
		"edits": []map[string]any{{"oldText": "world", "newText": "gopher"}},
	})
	preview, err := tool.Preview(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	text := preview.Content[0].(provider.TextBlock).Text
	for _, want := range []string{"-hello world", "+hello gopher"} {
		if !strings.Contains(text, want) {
			t.Fatalf("preview missing %q:\n%s", want, text)
		}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello world\n" {
		t.Fatalf("preview modified file: %q", b)
	}

	result, err := tool.Execute(context.Background(), args, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Content[0].(provider.TextBlock).Text; got != text {
		t.Fatalf("executed diff differs from preview:\npreview:\n%s\nresult:\n%s", text, got)
	}
}

func TestEditMultiple(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("a\nb\nc\n"), 0o644)
	tool := &EditTool{CWD: dir}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"path": "a.txt",
		"edits": []map[string]any{
			{"oldText": "a", "newText": "A"},
			{"oldText": "c", "newText": "C"},
		},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "A\nb\nC\n" {
		t.Fatalf("got %q", string(b))
	}
}

func TestEditAmbiguous(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("x\nx\n"), 0o644)
	tool := &EditTool{CWD: dir}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"path":  "a.txt",
		"edits": []map[string]any{{"oldText": "x", "newText": "y"}},
	}), nil)
	if err == nil {
		t.Fatal("want ambiguous error")
	}
}

func TestEditGuidance(t *testing.T) {
	desc := (&EditTool{}).Description()
	for _, want := range []string{"Inspect that file", "directly from its current contents", "short excerpts", "write"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description missing %q: %q", want, desc)
		}
	}

	var schema map[string]any
	if err := json.Unmarshal((&EditTool{}).Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	edits := properties["edits"].(map[string]any)
	items := edits["items"].(map[string]any)
	editProperties := items["properties"].(map[string]any)
	oldText := editProperties["oldText"].(map[string]any)
	if got, _ := oldText["description"].(string); !strings.Contains(got, "file being modified") {
		t.Fatalf("oldText schema description missing target-file guidance: %q", got)
	}
}

func TestEditNotFoundGuidesRecovery(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "destination.txt")
	if err := os.WriteFile(p, []byte("destination content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &EditTool{CWD: dir}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"path":  "destination.txt",
		"edits": []map[string]any{{"oldText": "content from another file", "newText": "replacement"}},
	}), nil)
	if err == nil {
		t.Fatal("want oldText not found error")
	}
	for _, want := range []string{"oldText not found", "destination.txt", "inspect that file again", "matching spaces and line breaks"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %q", want, err)
		}
	}
}

func TestEditPreservesCRLF(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("hello\r\nworld\r\n"), 0o644)
	tool := &EditTool{CWD: dir}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"path":  "a.txt",
		"edits": []map[string]any{{"oldText": "world", "newText": "gopher"}},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "hello\r\ngopher\r\n" {
		t.Fatalf("got %q", string(b))
	}
}

func TestResolveShell(t *testing.T) {
	notFound := errors.New("not found")
	tests := []struct {
		name     string
		goos     string
		binBash  bool
		pathBash string
		pathErr  error
		want     shellCommand
	}{
		{
			name: "prefers /bin/bash",
			goos: "linux", binBash: true, pathBash: "/usr/local/bin/bash",
			want: shellCommand{path: "/bin/bash", flag: "-c", isBash: true},
		},
		{
			name: "uses bash from PATH",
			goos: "linux", pathBash: "/usr/local/bin/bash",
			want: shellCommand{path: "/usr/local/bin/bash", flag: "-c", isBash: true},
		},
		{
			name: "falls back to POSIX sh",
			goos: "linux", pathErr: notFound,
			want: shellCommand{path: "/bin/sh", flag: "-c"},
		},
		{
			name: "uses Command Prompt on Windows",
			goos: "windows", binBash: true, pathBash: "/usr/local/bin/bash",
			want: shellCommand{path: "cmd", flag: "/C"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveShell(tt.goos, func(path string) bool {
				return path == "/bin/bash" && tt.binBash
			}, func(name string) (string, error) {
				if name != "bash" {
					t.Fatalf("unexpected PATH lookup for %q", name)
				}
				return tt.pathBash, tt.pathErr
			})
			if got != tt.want {
				t.Fatalf("resolveShell() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestShellDescription(t *testing.T) {
	tests := []struct {
		name  string
		shell shellCommand
		want  string
	}{
		{
			name:  "bash",
			shell: shellCommand{path: "/opt/bin/bash", flag: "-c", isBash: true},
			want:  "Run a Bash command via /opt/bin/bash -c. stdout+stderr merged.",
		},
		{
			name:  "POSIX fallback",
			shell: shellCommand{path: "/bin/sh", flag: "-c"},
			want:  "Bash is unavailable; run a POSIX sh command via /bin/sh -c. stdout+stderr merged.",
		},
		{
			name:  "Windows",
			shell: shellCommand{path: "cmd", flag: "/C"},
			want:  "Run a Windows Command Prompt command via cmd /C. stdout+stderr merged.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shellDescription(tt.shell); got != tt.want {
				t.Fatalf("shellDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBashSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell only")
	}
	tool := &BashTool{CWD: t.TempDir()}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "echo hi"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Content[0].(provider.TextBlock).Text
	if !strings.Contains(got, "hi") || !strings.Contains(got, "[exit 0]") {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "Took ") {
		t.Fatalf("bash output still contains duplicate timing: %q", got)
	}
	if details, ok := res.Details.(map[string]any); ok {
		if _, exists := details["duration_ms"]; exists {
			t.Fatalf("bash details still contain duplicate duration: %#v", details)
		}
	}
	if res.IsError {
		t.Fatal("unexpected error flag")
	}
}

func TestBashSyntax(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash syntax is not used on Windows")
	}
	if !currentShell().isBash {
		t.Skip("bash is unavailable")
	}
	tool := &BashTool{CWD: t.TempDir()}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"command": `items=(one two); [[ ${#items[@]} -eq 2 ]] && printf 'bash syntax works\n'`,
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Content[0].(provider.TextBlock).Text
	if res.IsError || !strings.Contains(got, "bash syntax works") {
		t.Fatalf("got %q", got)
	}
}

func TestBashFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell only")
	}
	tool := &BashTool{CWD: t.TempDir()}
	res, _ := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "false"}), nil)
	if !res.IsError {
		t.Fatal("want error")
	}
	got := res.Content[0].(provider.TextBlock).Text
	if !strings.Contains(got, "[exit 1]") {
		t.Fatalf("got %q", got)
	}
}
