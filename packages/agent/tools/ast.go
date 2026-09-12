package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const maxASTOutputBytes = 60 * 1024

// ASTTool performs structural searches through an installed ast-grep binary.
// Keeping the parser external preserves zut's portable, grammar-free binary.
type ASTTool struct {
	CWD      string
	Sandbox  *Sandbox
	LookPath func(string) (string, error)
}

type astArgs struct {
	Pattern  string `json:"pattern"`
	Language string `json:"language"`
	Path     string `json:"path"`
}

const astSchema = `{"type":"object","properties":{"pattern":{"type":"string","description":"ast-grep structural pattern, such as 'fmt.Errorf($MSG, $$$ARGS)'."},"language":{"type":"string","description":"ast-grep language id, such as go, rust, python, or typescript."},"path":{"type":"string","description":"File or directory to search, relative to the workspace or absolute."}},"required":["pattern","language","path"],"additionalProperties":false}`

func (t *ASTTool) Name() string { return "ast" }
func (t *ASTTool) Description() string {
	return "Search source code for an AST pattern using the installed ast-grep (sg) command. Output is bounded and execution never invokes a shell."
}
func (t *ASTTool) Schema() json.RawMessage { return json.RawMessage(astSchema) }

func (t *ASTTool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	var args astArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return core.ToolResult{}, fmt.Errorf("ast: invalid arguments: %w", err)
	}
	args.Pattern = strings.TrimSpace(args.Pattern)
	args.Language = strings.TrimSpace(args.Language)
	if args.Pattern == "" || args.Language == "" || strings.TrimSpace(args.Path) == "" {
		return core.ToolResult{}, fmt.Errorf("ast: pattern, language, and path are required")
	}
	path := resolvePath(t.CWD, args.Path)
	if err := t.Sandbox.CheckReadPath(path); err != nil {
		return core.ToolResult{}, err
	}
	lookPath := t.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	binary, err := lookPath("sg")
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("ast: ast-grep executable 'sg' not found in PATH")
	}
	cmd := exec.CommandContext(ctx, binary, "run", "--pattern", args.Pattern, "--lang", args.Language, "--json=stream", path)
	cmd.Dir = t.CWD
	if cmd.Dir == "" {
		cmd.Dir = filepath.Dir(path)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		return core.ToolResult{}, ctx.Err()
	}
	// ast-grep uses exit code 1 for no matches.
	if err != nil {
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			message := strings.TrimSpace(stderr.String())
			if message == "" {
				message = err.Error()
			}
			return core.ToolResult{}, fmt.Errorf("ast: %s", message)
		}
	}
	text := strings.TrimSpace(stdout.String())
	if text == "" {
		text = "No structural matches."
	}
	text = boundASTText(text)
	discovered := astResultPaths(stdout.Bytes(), path)
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: text}},
		Context: provider.ToolContext{Discovers: discovered},
		Details: map[string]any{"engine": "ast-grep", "path": path, "language": args.Language, "matches": len(discovered)},
	}, nil
}

func boundASTText(text string) string {
	if len(text) <= maxASTOutputBytes {
		return text
	}
	suffix := fmt.Sprintf("\n... [truncated at %d bytes]", maxASTOutputBytes)
	text = text[:maxASTOutputBytes-len(suffix)]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text + suffix
}

func astResultPaths(output []byte, searchPath string) []string {
	seen := make(map[string]bool)
	searchInfo, _ := os.Stat(searchPath)
	var paths []string
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		var result struct {
			File string `json:"file"`
		}
		if json.Unmarshal(line, &result) != nil || result.File == "" {
			continue
		}
		path := result.File
		if !filepath.IsAbs(path) {
			base := searchPath
			if searchInfo == nil || !searchInfo.IsDir() {
				base = filepath.Dir(searchPath)
			}
			path = filepath.Join(base, path)
		}
		path = filepath.Clean(path)
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	return paths
}
