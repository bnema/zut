package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/bnema/zut/packages/agent/lsp"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// EditTool applies one or more exact-match replacements to a file.
type EditTool struct {
	CWD            string
	Sandbox        *Sandbox
	LSP            *lsp.Manager
	LSPDiagnostics bool
}

type editOp struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

type editArgs struct {
	Path  string   `json:"path"`
	Edits []editOp `json:"edits"`
}

const editSchema = `{"type":"object","properties":{"path":{"type":"string","description":"File to modify, given as an absolute path or one relative to the working directory."},"edits":{"type":"array","description":"A set of non-overlapping substitutions, all located in the file content as it existed before this call.","items":{"type":"object","properties":{"oldText":{"type":"string","description":"A verbatim excerpt from the file being modified with enough surrounding text to identify exactly one location, without intersecting another edit."},"newText":{"type":"string","description":"Content that will replace the matched excerpt."}},"required":["oldText","newText"]}}},"required":["path","edits"]}`

func (t *EditTool) Name() string { return "edit" }
func (t *EditTool) Description() string {
	return "Apply exact substitutions to an existing file. Inspect that file before editing and take every oldText directly from its current contents. Use a verbatim excerpt with enough surrounding text to identify exactly one location; choose write when replacing most or all of a file."
}
func (t *EditTool) Schema() json.RawMessage { return json.RawMessage(editSchema) }

type editPlan struct {
	path   string
	final  []byte
	result core.ToolResult
}

// Preview validates the edit against the current file and returns the exact
// diff without writing anything. The confirmation UI shows this result before
// the user decides whether Execute may apply it.
func (t *EditTool) Preview(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	plan, err := t.plan(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	return plan.result, nil
}

func (t *EditTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	plan, err := t.plan(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	if err := os.WriteFile(plan.path, plan.final, 0o644); err != nil {
		return core.ToolResult{}, err
	}
	if t.LSPDiagnostics {
		attachWriteDiagnostics(ctx, t.CWD, plan.path, t.LSP, &plan.result)
	}
	return plan.result, nil
}

func (t *EditTool) plan(raw json.RawMessage) (editPlan, error) {
	var a editArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return editPlan{}, fmt.Errorf("invalid args: %w", err)
	}
	if a.Path == "" {
		return editPlan{}, fmt.Errorf("path is required")
	}
	if len(a.Edits) == 0 {
		return editPlan{}, fmt.Errorf("at least one edit is required")
	}
	path := resolvePath(t.CWD, a.Path)
	if err := t.Sandbox.CheckWritePath(path); err != nil {
		return editPlan{}, err
	}

	orig, err := os.ReadFile(path)
	if err != nil {
		return editPlan{}, err
	}

	// Detect BOM and line endings.
	var bom []byte
	if bytes.HasPrefix(orig, []byte{0xEF, 0xBB, 0xBF}) {
		bom = orig[:3]
		orig = orig[3:]
	}
	nl := detectLineEnding(orig)
	// Normalize the file and edit operands to \n for matching and replacement.
	body := normalizeLineEndings(string(orig))

	// Validate all edits against the original content and build their spans in
	// the same pass. This keeps validation and application anchored to the same
	// match instead of locating each oldText a second time.
	type span struct {
		start, end  int
		replacement string
	}
	spans := make([]span, 0, len(a.Edits))
	for i, e := range a.Edits {
		oldText := normalizeLineEndings(e.OldText)
		newText := normalizeLineEndings(e.NewText)
		if oldText == "" {
			return editPlan{}, fmt.Errorf("edit %d: oldText must not be empty", i+1)
		}
		if oldText == newText {
			return editPlan{}, fmt.Errorf("edit %d: oldText equals newText", i+1)
		}
		matches := summarizeMatches(body, oldText)
		if matches.count == 0 {
			return editPlan{}, fmt.Errorf("edit %d: oldText not found in %s; inspect that file again and use a verbatim excerpt with matching spaces and line breaks", i+1, a.Path)
		}
		if matches.count > 1 {
			return editPlan{}, fmt.Errorf("edit %d: oldText matches %d times (must be unique) in %s at %s; read %s with a bounded context window around those lines, then retry with a verbatim excerpt containing enough surrounding text to identify exactly one location", i+1, matches.count, a.Path, formatMatchLines(matches.lines, matches.count), a.Path)
		}
		spans = append(spans, span{
			start:       matches.start,
			end:         matches.start + len(oldText),
			replacement: newText,
		})
	}

	// Apply atomically (sorted by position so offsets stay valid as we splice).
	// Check for overlaps.
	for i := 0; i < len(spans); i++ {
		for j := i + 1; j < len(spans); j++ {
			if spans[i].start < spans[j].end && spans[j].start < spans[i].end {
				return editPlan{}, fmt.Errorf("edits overlap; merge them into one edit")
			}
		}
	}
	// Sort ascending.
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j-1].start > spans[j].start; j-- {
			spans[j-1], spans[j] = spans[j], spans[j-1]
		}
	}
	var out strings.Builder
	prev := 0
	for _, s := range spans {
		out.WriteString(body[prev:s.start])
		out.WriteString(s.replacement)
		prev = s.end
	}
	out.WriteString(body[prev:])
	newBody := out.String()

	// Restore line endings.
	if nl == "\r\n" {
		newBody = strings.ReplaceAll(newBody, "\n", "\r\n")
	}
	final := append([]byte{}, bom...)
	final = append(final, []byte(newBody)...)

	diff := unifiedDiff(a.Path, string(orig), strings.ReplaceAll(newBody, "\r\n", "\n"))
	// The tool-call header renders the path above the result, so the
	// result body is just the context diff. Details carries metadata for
	// programmatic consumers and confirmation previews.
	result := core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: diff}},
		Details: map[string]any{"path": path, "edits": len(a.Edits), "diff": diff},
	}
	return editPlan{path: path, final: final, result: result}, nil
}

const maxReportedMatchLines = 5

type matchSummary struct {
	count int
	start int
	lines []int
}

// summarizeMatches counts every possible match start, including overlaps,
// while retaining only the line numbers useful for recovery guidance.
func summarizeMatches(body, oldText string) matchSummary {
	var summary matchSummary
	for searchFrom := 0; searchFrom < len(body); {
		relative := strings.Index(body[searchFrom:], oldText)
		if relative < 0 {
			break
		}
		start := searchFrom + relative
		if summary.count == 0 {
			summary.start = start
		}
		summary.count++
		if len(summary.lines) < maxReportedMatchLines {
			summary.lines = append(summary.lines, 1+strings.Count(body[:start], "\n"))
		}
		searchFrom = start + 1
	}
	return summary
}

func formatMatchLines(lines []int, count int) string {
	parts := make([]string, len(lines))
	for i, line := range lines {
		parts[i] = fmt.Sprintf("%d", line)
	}

	var list string
	switch len(parts) {
	case 1:
		list = parts[0]
	case 2:
		list = parts[0] + " and " + parts[1]
	default:
		list = strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
	}
	if omitted := count - len(lines); omitted > 0 {
		list += fmt.Sprintf(" (and %d more)", omitted)
	}
	return "lines " + list
}

func normalizeLineEndings(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func detectLineEnding(b []byte) string {
	if bytes.Contains(b, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

// diffContextLines is the number of unchanged lines kept on each
// side of an edit when rendering the diff. 3 is the git-diff
// default and balances readability with transcript size.
const diffContextLines = 3

// unifiedDiff emits a context diff for the edit tool's result.
//
// Shape: each output row is either
//   - " <line>"       unchanged context
//   - "-<line>"       deletion (from a)
//   - "+<line>"       addition (to b)
//   - "..."           context break between hunks
//
// The legacy "--- name / +++ name" header is omitted because the
// tool-call header above the result already shows the path. Only
// lines within diffContextLines of a +/- row are kept; longer
// runs of unchanged content collapse into a single "..." row so
// a one-line edit in a thousand-line file produces a short
// transcript.
func unifiedDiff(name, a, b string) string {
	if a == b {
		return ""
	}
	aLines := strings.Split(a, "\n")
	bLines := strings.Split(b, "\n")
	ops := diffLines(aLines, bLines)

	// Mark ops that sit within diffContextLines of any +/- op.
	keep := make([]bool, len(ops))
	for i, op := range ops {
		if op.kind == '+' || op.kind == '-' {
			keep[i] = true
			for d := 1; d <= diffContextLines; d++ {
				if i-d >= 0 {
					keep[i-d] = true
				}
				if i+d < len(ops) {
					keep[i+d] = true
				}
			}
		}
	}

	var sb strings.Builder
	prevKept := false
	anyOutput := false
	for i, op := range ops {
		if !keep[i] {
			if prevKept {
				sb.WriteString("...\n")
				prevKept = false
			}
			continue
		}
		if !prevKept && anyOutput {
			sb.WriteString("...\n")
		}
		switch op.kind {
		case ' ':
			fmt.Fprintf(&sb, " %s\n", op.line)
		case '-':
			fmt.Fprintf(&sb, "-%s\n", op.line)
		case '+':
			fmt.Fprintf(&sb, "+%s\n", op.line)
		}
		prevKept = true
		anyOutput = true
	}
	_ = name // header dropped; kept in signature for call-site stability
	return sb.String()
}

type diffOp struct {
	kind byte
	line string
}

func diffLines(a, b []string) []diffOp {
	// LCS table.
	m, n := len(a), len(b)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 0; i < m; i++ {
		for j := 0; j < n; j++ {
			if a[i] == b[j] {
				dp[i+1][j+1] = dp[i][j] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i+1][j+1] = dp[i+1][j]
			} else {
				dp[i+1][j+1] = dp[i][j+1]
			}
		}
	}
	// Backtrack.
	var ops []diffOp
	i, j := m, n
	for i > 0 && j > 0 {
		if a[i-1] == b[j-1] {
			ops = append([]diffOp{{' ', a[i-1]}}, ops...)
			i--
			j--
		} else if dp[i][j-1] >= dp[i-1][j] {
			ops = append([]diffOp{{'+', b[j-1]}}, ops...)
			j--
		} else {
			ops = append([]diffOp{{'-', a[i-1]}}, ops...)
			i--
		}
	}
	for i > 0 {
		ops = append([]diffOp{{'-', a[i-1]}}, ops...)
		i--
	}
	for j > 0 {
		ops = append([]diffOp{{'+', b[j-1]}}, ops...)
		j--
	}
	return ops
}
