package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bnema/zut/packages/agent/lsp"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const maxASTOutputBytes = 60 * 1024

// ASTTool performs structural searches and rewrites through ast-grep.
type ASTTool struct {
	CWD            string
	Sandbox        *Sandbox
	LSP            *lsp.Manager
	LSPDiagnostics bool
	LookPath       func(string) (string, error)
}

var (
	_ core.Tool          = (*ASTTool)(nil)
	_ core.ToolPreviewer = (*ASTTool)(nil)
)

type astArgs struct {
	Pattern  string `json:"pattern"`
	Language string `json:"language"`
	Path     string `json:"path"`
	Rewrite  string `json:"rewrite,omitempty"`
}

const astSchema = `{"type":"object","properties":{"pattern":{"type":"string","description":"ast-grep structural pattern, such as '$A($$$ARGS)' for a call."},"language":{"type":"string","description":"ast-grep language id, such as go, rust, python, or typescript."},"path":{"type":"string","description":"File or directory to search, relative to the workspace or absolute."},"rewrite":{"type":"string","description":"Optional structural replacement. When supplied, Zut previews and safely applies every match."}},"required":["pattern","language","path"],"additionalProperties":false}`

func (t *ASTTool) Name() string { return "ast" }
func (t *ASTTool) Description() string {
	return "Prefer this for syntax-aware code searches and rewrites. Uses installed ast-grep, previews mutations, enforces filesystem scope, and attaches diagnostics. Use grep/edit for plain text."
}
func (t *ASTTool) Schema() json.RawMessage { return json.RawMessage(astSchema) }

func (t *ASTTool) Preview(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	args, err := parseASTArgs(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	if args.Rewrite == "" {
		return core.ToolResult{}, fmt.Errorf("ast: preview requires rewrite")
	}
	plan, err := t.plan(ctx, args)
	if err != nil {
		return core.ToolResult{}, err
	}
	return plan.result, nil
}

func (t *ASTTool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	args, err := parseASTArgs(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	plan, err := t.plan(ctx, args)
	if err != nil {
		return core.ToolResult{}, err
	}
	if args.Rewrite == "" {
		return plan.result, nil
	}
	for _, change := range plan.changes {
		if err := os.WriteFile(change.path, change.content, change.mode); err != nil {
			return core.ToolResult{}, fmt.Errorf("ast: write %s: %w", change.path, err)
		}
	}
	if t.LSPDiagnostics {
		attachMutationDiagnostics(ctx, t.CWD, plan.result.Context.Mutates, t.LSP, &plan.result)
	}
	return plan.result, nil
}

type astChange struct {
	path    string
	content []byte
	mode    os.FileMode
}

type astPlan struct {
	result  core.ToolResult
	changes []astChange
}

type astMatch struct {
	Text        string `json:"text"`
	File        string `json:"file"`
	Lines       string `json:"lines"`
	Replacement string `json:"replacement"`
	Range       struct {
		ByteOffset struct {
			Start int `json:"start"`
			End   int `json:"end"`
		} `json:"byteOffset"`
		Start struct {
			Line int `json:"line"`
		} `json:"start"`
	} `json:"range"`
}

func parseASTArgs(raw json.RawMessage) (astArgs, error) {
	var args astArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return args, fmt.Errorf("ast: invalid arguments: %w", err)
	}
	args.Pattern = strings.TrimSpace(args.Pattern)
	args.Language = strings.TrimSpace(args.Language)
	if args.Pattern == "" || args.Language == "" || strings.TrimSpace(args.Path) == "" {
		return args, fmt.Errorf("ast: pattern, language, and path are required")
	}
	return args, nil
}

func (t *ASTTool) plan(ctx context.Context, args astArgs) (astPlan, error) {
	root, err := canonicalPath(t.CWD, args.Path)
	if err != nil {
		return astPlan{}, fmt.Errorf("ast: resolve path: %w", err)
	}
	if args.Rewrite == "" {
		if err := t.Sandbox.CheckReadPath(root); err != nil {
			return astPlan{}, err
		}
	} else if err := t.Sandbox.CheckWritePath(root); err != nil {
		return astPlan{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return astPlan{}, fmt.Errorf("ast: %w", err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return astPlan{}, fmt.Errorf("ast: %s is not a regular file or directory", args.Path)
	}
	binary, err := t.executable()
	if err != nil {
		return astPlan{}, err
	}
	commandArgs := []string{"run", "--pattern", args.Pattern, "--lang", args.Language}
	if args.Rewrite != "" {
		commandArgs = append(commandArgs, "--rewrite", args.Rewrite)
	}
	commandArgs = append(commandArgs, "--json=stream", root)
	cmd := exec.CommandContext(ctx, binary, commandArgs...)
	cmd.Dir = root
	if !info.IsDir() {
		cmd.Dir = filepath.Dir(root)
	}
	configureBashProcess(cmd, nil)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return astPlan{}, ctx.Err()
	}
	if runErr != nil {
		var exit *exec.ExitError
		if !errors.As(runErr, &exit) || exit.ExitCode() != 1 || strings.TrimSpace(stderr.String()) != "" || stdout.Len() != 0 {
			message := strings.TrimSpace(stderr.String())
			if message == "" {
				message = runErr.Error()
			}
			return astPlan{}, fmt.Errorf("ast: %s", message)
		}
	}
	matches, err := decodeASTMatches(stdout.Bytes())
	if err != nil {
		return astPlan{}, err
	}
	for index := range matches {
		if !filepath.IsAbs(matches[index].File) {
			matches[index].File = filepath.Join(cmd.Dir, matches[index].File)
		}
		matches[index].File = filepath.Clean(matches[index].File)
	}
	if args.Rewrite == "" {
		text, paths := formatASTMatches(matches)
		return astPlan{result: core.ToolResult{
			Content: []provider.Content{provider.TextBlock{Text: text}},
			Context: provider.ToolContext{Discovers: paths},
			Details: map[string]any{"engine": "ast-grep", "path": root, "language": args.Language, "matches": len(matches)},
		}}, nil
	}
	return t.planRewrite(root, args, matches)
}

func (t *ASTTool) executable() (string, error) {
	lookup := t.LookPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	for _, name := range []string{"ast-grep", "sg"} {
		if path, err := lookup(name); err == nil && path != "" {
			return path, nil
		}
	}
	return "", fmt.Errorf("ast: ast-grep executable ('ast-grep' or 'sg') not found in PATH")
}

func decodeASTMatches(output []byte) ([]astMatch, error) {
	var matches []astMatch
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		var match astMatch
		if err := json.Unmarshal(scanner.Bytes(), &match); err != nil {
			return nil, fmt.Errorf("ast: decode result: %w", err)
		}
		if match.File != "" {
			matches = append(matches, match)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("ast: decode output: %w", err)
	}
	return matches, nil
}

func formatASTMatches(matches []astMatch) (string, []string) {
	seen := make(map[string]bool)
	var paths []string
	var output strings.Builder
	for _, match := range matches {
		path := filepath.Clean(match.File)
		fmt.Fprintf(&output, "%s:%d: %s\n", path, match.Range.Start.Line+1, strings.TrimSpace(match.Lines))
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	if len(matches) == 0 {
		return "No structural matches.", nil
	}
	return boundASTText(strings.TrimSpace(output.String())), paths
}

func (t *ASTTool) planRewrite(root string, args astArgs, matches []astMatch) (astPlan, error) {
	byFile := make(map[string][]astMatch)
	for _, match := range matches {
		path, err := canonicalPath(t.CWD, match.File)
		if err != nil {
			return astPlan{}, err
		}
		if err := t.Sandbox.CheckWritePath(path); err != nil {
			return astPlan{}, err
		}
		byFile[path] = append(byFile[path], match)
	}
	paths := make([]string, 0, len(byFile))
	for path := range byFile {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var changes []astChange
	var diffs strings.Builder
	for _, path := range paths {
		original, err := os.ReadFile(path)
		if err != nil {
			return astPlan{}, fmt.Errorf("ast: read %s: %w", path, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return astPlan{}, err
		}
		updated := append([]byte(nil), original...)
		fileMatches := byFile[path]
		sort.Slice(fileMatches, func(i, j int) bool {
			return fileMatches[i].Range.ByteOffset.Start > fileMatches[j].Range.ByteOffset.Start
		})
		lastStart := len(updated)
		for _, match := range fileMatches {
			start, end := match.Range.ByteOffset.Start, match.Range.ByteOffset.End
			if start < 0 || end < start || end > len(updated) || end > lastStart {
				return astPlan{}, fmt.Errorf("ast: invalid or overlapping rewrite range in %s", path)
			}
			updated = append(updated[:start], append([]byte(match.Replacement), updated[end:]...)...)
			lastStart = start
		}
		if bytes.Equal(original, updated) {
			continue
		}
		diffs.WriteString(unifiedDiff(path, string(original), string(updated)))
		changes = append(changes, astChange{path: path, content: updated, mode: info.Mode().Perm()})
	}
	text := boundASTText(diffs.String())
	if len(changes) == 0 {
		text = "No structural matches."
	}
	mutates := make([]string, 0, len(changes))
	for _, change := range changes {
		mutates = append(mutates, change.path)
	}
	return astPlan{changes: changes, result: core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: text}},
		Context: provider.ToolContext{Mutates: mutates},
		Details: map[string]any{"engine": "ast-grep", "path": root, "language": args.Language, "matches": len(matches), "files": len(changes), "diff": text},
	}}, nil
}

func canonicalPath(cwd, path string) (string, error) {
	path = resolvePath(cwd, path)
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func boundASTText(text string) string {
	if len(text) <= maxASTOutputBytes {
		return text
	}
	suffix := fmt.Sprintf("\n... [truncated at %d bytes]", maxASTOutputBytes)
	text = text[:maxASTOutputBytes-len(suffix)]
	for len(text) > 0 && !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text + suffix
}
