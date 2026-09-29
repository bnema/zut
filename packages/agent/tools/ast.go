package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bnema/zut/packages/agent/lsp"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const (
	maxASTOutputBytes  = 60 * 1024
	maxASTCaptureBytes = 4 * 1024 * 1024
)

// ASTTool performs structural searches and rewrites through ast-grep.
type ASTTool struct {
	CWD            string
	Sandbox        *Sandbox
	LSP            *lsp.Manager
	LSPDiagnostics bool
	ReadOnly       bool
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

const astSchema = `{"type":"object","properties":{"pattern":{"type":"string","description":"Code-shaped ast-grep pattern. $NAME matches one node, $$$NAME matches zero or more nodes (arguments, statements)."},"language":{"type":"string","description":"Optional ast-grep language id (go, rust, python, typescript, tsx, javascript, ...). Omit to infer it from each file's extension."},"path":{"type":"string","description":"File or directory to search, relative to the workspace or absolute."},"rewrite":{"type":"string","description":"Optional replacement reusing captured metavariables. Zut previews the diff and applies every match."}},"required":["pattern","path"],"additionalProperties":false}`

func (t *ASTTool) Name() string { return "ast" }
func (t *ASTTool) Description() string {
	return "Structural code search and rewrite (ast-grep). Prefer it over grep/edit when looking for code shapes: calls, definitions, statements, or a rename/refactor across many places. " +
		"Matches ignore formatting and span multiple lines. Examples: " +
		"calls `fmt.Errorf($MSG, $$$REST)`; " +
		"Go methods `func ($R $T) $NAME($$$) $$$ { $$$ }`; " +
		"error checks `if err != nil { $$$ }`; " +
		"JS `console.log($$$)`; " +
		"rename: pattern `oldFn($$$A)` with rewrite `newFn($$$A)`. " +
		"Use grep for plain text, comments, or strings."
}
func (t *ASTTool) Schema() json.RawMessage { return json.RawMessage(astSchema) }

func (t *ASTTool) Preview(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	args, err := parseASTArgs(raw)
	if err != nil {
		return core.ToolResult{}, err
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
	if err := t.applyRewrite(plan.changes); err != nil {
		return core.ToolResult{}, err
	}
	if t.LSPDiagnostics {
		attachMutationDiagnostics(ctx, t.CWD, plan.result.Context.Mutates, t.LSP, &plan.result)
	}
	return plan.result, nil
}

func (t *ASTTool) applyRewrite(changes []astChange) error {
	// Validate the entire plan before starting any writes, so a stale plan
	// is rejected without touching any file.
	for _, change := range changes {
		if _, err := t.validateASTChange(change); err != nil {
			return err
		}
	}
	var written []string
	for _, change := range changes {
		// Validate again immediately before replacing each file: earlier
		// writes take time, and a symlink may have been retargeted.
		target, err := t.validateASTChange(change)
		if err == nil {
			err = writeASTChange(target, change)
		}
		if err != nil {
			if len(written) == 0 {
				return err
			}
			return fmt.Errorf("%w (already written: %s)", err, strings.Join(written, ", "))
		}
		written = append(written, change.path)
	}
	return nil
}

// validateASTChange resolves the file that will be replaced, checks it
// against the write scope, and confirms it still holds the planned bytes.
func (t *ASTTool) validateASTChange(change astChange) (string, error) {
	if err := t.Sandbox.CheckWritePath(change.path); err != nil {
		return "", err
	}
	// Rename replaces a symlink itself, so write through to its target like
	// os.WriteFile does, and check that target too.
	target, err := filepath.EvalSymlinks(change.path)
	if err != nil {
		return "", fmt.Errorf("ast: resolve %s: %w", change.path, err)
	}
	if err := t.Sandbox.CheckWritePath(target); err != nil {
		return "", err
	}
	current, err := os.ReadFile(target)
	if err != nil {
		return "", fmt.Errorf("ast: re-read %s: %w", change.path, err)
	}
	if !bytes.Equal(current, change.original) {
		return "", fmt.Errorf("ast: %s changed while applying rewrite", change.path)
	}
	return target, nil
}

type astChange struct {
	path     string
	original []byte
	content  []byte
	mode     os.FileMode
}

// writeASTChange replaces target through a temporary file in the same
// directory, so a failed write never leaves a truncated file.
func writeASTChange(target string, change astChange) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".zut-ast-*")
	if err != nil {
		return fmt.Errorf("ast: write %s: %w", change.path, err)
	}
	defer os.Remove(tmp.Name())
	err = tmp.Chmod(change.mode)
	if err == nil {
		_, err = tmp.Write(change.content)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), target)
	}
	if err != nil {
		return fmt.Errorf("ast: write %s: %w", change.path, err)
	}
	return nil
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
	if args.Pattern == "" || strings.TrimSpace(args.Path) == "" {
		return args, fmt.Errorf("ast: pattern and path are required")
	}
	return args, nil
}

func (t *ASTTool) plan(ctx context.Context, args astArgs) (astPlan, error) {
	if t.ReadOnly && args.Rewrite != "" {
		return astPlan{}, fmt.Errorf("ast: rewrites are unavailable in read-only orchestration")
	}
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
	dir := root
	if !info.IsDir() {
		dir = filepath.Dir(root)
	}
	commandArgs := []string{"run", "--pattern=" + args.Pattern}
	if args.Language != "" {
		commandArgs = append(commandArgs, "--lang="+args.Language)
	}
	if args.Rewrite != "" {
		commandArgs = append(commandArgs, "--rewrite="+args.Rewrite)
	}
	commandArgs = append(commandArgs, "--json=stream", root)
	run, err := runASTGrep(ctx, binary, dir, commandArgs, args.Rewrite != "")
	if err != nil {
		return astPlan{}, err
	}
	if len(run.matches) == 0 && astGoCallPattern(args, root) {
		// tree-sitter-go parses a bare `pkg.Func(...)` pattern as a type
		// conversion, so it never matches real calls. Retry it as a call
		// expression inside a function body. A zero-match retry keeps the
		// original result and its warnings; a failed retry is reported.
		retry, err := runASTGrep(ctx, binary, dir, astGoCallRuleArgs(args, root), args.Rewrite != "")
		if err != nil {
			return astPlan{}, fmt.Errorf("%w (while retrying the Go call pattern as a call expression)", err)
		}
		if len(retry.matches) > 0 {
			run = retry
		}
	}
	matches, stderr := run.matches, run.stderr
	for index := range matches {
		if !filepath.IsAbs(matches[index].File) {
			matches[index].File = filepath.Join(dir, matches[index].File)
		}
		matches[index].File = filepath.Clean(matches[index].File)
	}
	if args.Rewrite == "" {
		text, paths := formatASTMatches(matches)
		if len(matches) == 0 {
			text += astWarning(stderr) + astNoMatchHint
		}
		if run.overflow {
			text += fmt.Sprintf("\n... [search output truncated at %d bytes; narrow path or pattern for complete results]", maxASTCaptureBytes)
		}
		return astPlan{result: core.ToolResult{
			Content: []provider.Content{provider.TextBlock{Text: text}},
			Context: provider.ToolContext{Discovers: paths},
			Details: map[string]any{"engine": "ast-grep", "path": root, "language": args.Language, "matches": len(matches)},
		}}, nil
	}
	plan, err := t.planRewrite(root, args, matches)
	if err == nil && len(matches) == 0 {
		text := plan.result.Content[0].(provider.TextBlock).Text + astWarning(stderr) + astNoMatchHint
		plan.result.Content = []provider.Content{provider.TextBlock{Text: text}}
		plan.result.Details.(map[string]any)["diff"] = text
	}
	return plan, err
}

// Available reports whether an ast-grep executable can be found.
func (t *ASTTool) Available() bool {
	_, err := t.executable()
	return err == nil
}

func (t *ASTTool) executable() (string, error) {
	lookup := t.LookPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	if path, err := lookup("ast-grep"); err == nil && path != "" {
		return path, nil
	}
	if path, err := lookup("sg"); err == nil && path != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
		if err == nil && strings.Contains(strings.ToLower(string(output)), "ast-grep") {
			return path, nil
		}
	}
	return "", fmt.Errorf("ast: ast-grep executable ('ast-grep' or 'sg') not found in PATH")
}

const astNoMatchHint = "\nTip: the pattern must be valid code in the target language; $A matches one node and $$$A matches zero or more. Set language when the path mixes languages."

type astRun struct {
	matches  []astMatch
	stderr   string
	overflow bool
}

// runASTGrep runs one ast-grep command and decodes its JSON stream. Exit
// status 1 with no output is ast-grep's normal "no matches" result.
func runASTGrep(ctx context.Context, binary, dir string, commandArgs []string, rewrite bool) (astRun, error) {
	cmd := exec.CommandContext(ctx, binary, commandArgs...)
	cmd.Dir = dir
	configureBashProcess(cmd, nil)
	var stdout, stderr bytes.Buffer
	capture := &limitedASTBuffer{buffer: &stdout, limit: maxASTCaptureBytes}
	cmd.Stdout = capture
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return astRun{}, ctx.Err()
	}
	if capture.overflow {
		if rewrite {
			return astRun{}, fmt.Errorf("ast: output exceeds %d bytes; use a narrower path or pattern", capture.limit)
		}
		// The capture can end mid-record. Only decode complete JSON lines.
		data := append([]byte(nil), stdout.Bytes()...)
		stdout.Reset()
		if end := bytes.LastIndexByte(data, '\n'); end >= 0 {
			_, _ = stdout.Write(data[:end+1])
		}
	}
	if runErr != nil {
		var exit *exec.ExitError
		if !errors.As(runErr, &exit) || exit.ExitCode() != 1 || strings.TrimSpace(stderr.String()) != "" || stdout.Len() != 0 {
			message := strings.TrimSpace(stderr.String())
			if message == "" {
				message = runErr.Error()
			}
			return astRun{}, fmt.Errorf("ast: %s", message)
		}
	}
	matches, err := decodeASTMatches(stdout.Bytes())
	if err != nil {
		return astRun{}, err
	}
	return astRun{matches: matches, stderr: stderr.String(), overflow: capture.overflow}, nil
}

var astGoQualifiedCall = regexp.MustCompile(`^[A-Za-z_$][\w$]*\.[A-Za-z_$][\w$]*\(.*\)$`)

// astGoCallPattern reports whether a Go search may have hit the tree-sitter
// ambiguity where `pkg.Func(x)` parses as a type conversion.
func astGoCallPattern(args astArgs, root string) bool {
	switch {
	case strings.EqualFold(args.Language, "go"), strings.EqualFold(args.Language, "golang"):
	case args.Language == "" && strings.EqualFold(filepath.Ext(root), ".go"):
	case args.Language == "" && filepath.Ext(root) == "":
	default:
		return false
	}
	return astGoQualifiedCall.MatchString(args.Pattern)
}

// astGoCallRuleArgs matches the pattern as a call expression inside a
// function body, which removes the type-conversion ambiguity.
func astGoCallRuleArgs(args astArgs, root string) []string {
	rule := map[string]any{
		"id":       "zut-ast",
		"language": "go",
		"rule": map[string]any{"pattern": map[string]any{
			"context":  "func _() { " + args.Pattern + " }",
			"selector": "call_expression",
		}},
	}
	if args.Rewrite != "" {
		rule["fix"] = args.Rewrite
	}
	encoded, _ := json.Marshal(rule)
	return []string{"scan", "--inline-rules=" + string(encoded), "--json=stream", root}
}

type limitedASTBuffer struct {
	buffer   *bytes.Buffer
	limit    int
	overflow bool
}

func (w *limitedASTBuffer) Write(p []byte) (int, error) {
	remaining := w.limit - w.buffer.Len()
	if remaining > len(p) {
		remaining = len(p)
	}
	if remaining > 0 {
		_, _ = w.buffer.Write(p[:remaining])
	}
	if remaining < len(p) {
		w.overflow = true
	}
	// Always consume the complete write so the child can exit normally. Returning
	// an error here closes its stdout pipe and masks this limit as "broken pipe".
	return len(p), nil
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

func astWarning(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	if len(stderr) > 2048 {
		stderr = stderr[:2048] + "... [warning truncated]"
	}
	return "\n" + stderr
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
			left, right := fileMatches[i].Range.ByteOffset, fileMatches[j].Range.ByteOffset
			if left.Start == right.Start {
				return left.End > right.End
			}
			return left.Start < right.Start
		})
		selected := make([]astMatch, 0, len(fileMatches))
		lastEnd := -1
		for _, match := range fileMatches {
			start, end := match.Range.ByteOffset.Start, match.Range.ByteOffset.End
			if start < 0 || end < start || end > len(original) {
				return astPlan{}, fmt.Errorf("ast: invalid rewrite range in %s", path)
			}
			if start < lastEnd {
				continue // ast-grep applies the outermost non-overlapping match.
			}
			if !bytes.Equal(original[start:end], []byte(match.Text)) {
				return astPlan{}, fmt.Errorf("ast: %s changed while planning rewrite", path)
			}
			selected = append(selected, match)
			lastEnd = end
		}
		for index := len(selected) - 1; index >= 0; index-- {
			match := selected[index]
			start, end := match.Range.ByteOffset.Start, match.Range.ByteOffset.End
			updated = append(updated[:start], append([]byte(match.Replacement), updated[end:]...)...)
		}
		if bytes.Equal(original, updated) {
			continue
		}
		if strings.EqualFold(args.Language, "go") || strings.EqualFold(args.Language, "golang") || (args.Language == "" && strings.EqualFold(filepath.Ext(path), ".go")) {
			crlf := bytes.Contains(original, []byte("\r\n"))
			normalized := bytes.ReplaceAll(original, []byte("\r\n"), []byte("\n"))
			// Mixed endings and pre-existing gofmt differences must not cause
			// unrelated lines to change as a side effect of this rewrite.
			if !crlf || bytes.Count(original, []byte("\n")) == bytes.Count(original, []byte("\r\n")) {
				if clean, err := format.Source(normalized); err == nil && bytes.Equal(clean, normalized) {
					formatted, err := format.Source(updated)
					if err == nil {
						if crlf {
							formatted = bytes.ReplaceAll(formatted, []byte("\n"), []byte("\r\n"))
						}
						updated = formatted
					}
				}
			}
		}
		if bytes.Equal(original, updated) {
			continue
		}
		diffs.WriteString(unifiedDiff(path, string(original), string(updated)))
		changes = append(changes, astChange{path: path, original: original, content: updated, mode: info.Mode().Perm()})
	}
	text := boundASTText(diffs.String())
	if len(changes) == 0 {
		if len(matches) == 0 {
			text = "No structural matches."
		} else {
			text = "Structural matches found, but the rewrite produced no changes."
		}
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
