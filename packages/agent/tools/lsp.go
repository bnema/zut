package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bnema/zut/packages/agent/lsp"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const (
	maxLSPToolOutput    = 60 * 1024
	defaultLSPTimeout   = 30 * time.Second
	maxLSPTimeout       = 2 * time.Minute
	maxLSPTimeoutMillis = int(maxLSPTimeout / time.Millisecond)
)

// LSPTool exposes diagnostics, language navigation, and server management to
// the model. Manager owns processes and is normally shared by all tool calls
// in one agent session.
type LSPTool struct {
	CWD            string
	Manager        *lsp.Manager
	Sandbox        *Sandbox
	LSPDiagnostics bool
}

// NewLSPTool constructs the model-facing LSP tool.
func NewLSPTool(cwd string, manager *lsp.Manager) *LSPTool {
	if manager == nil {
		manager = lsp.NewManager()
	}
	return &LSPTool{CWD: cwd, Manager: manager}
}

type lspArgs struct {
	Action  string          `json:"action"`
	Path    string          `json:"path,omitempty"`
	Line    int             `json:"line,omitempty"`
	Column  int             `json:"column,omitempty"`
	Query   string          `json:"query,omitempty"`
	NewName string          `json:"new_name,omitempty"`
	Server  string          `json:"server,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Apply   *bool           `json:"apply,omitempty"`
	Timeout int             `json:"timeout_ms,omitempty"`
	Max     int             `json:"max,omitempty"`
	RunCLI  *bool           `json:"run_cli,omitempty"`
}

const lspSchema = `{"type":"object","properties":{"action":{"type":"string","enum":["diagnostics","definition","references","hover","symbols","rename","code_actions","type_definition","implementation","status","reload","capabilities","request"]},"path":{"type":"string","description":"File path relative to the workspace, required for document actions."},"line":{"type":"integer","minimum":1,"description":"1-based line for a document action."},"column":{"type":"integer","minimum":1,"description":"1-based column for a document action."},"query":{"type":"string","description":"Workspace symbol query; symbols uses document symbols when omitted."},"new_name":{"type":"string","description":"New identifier for rename."},"server":{"type":"string","description":"Optional server id for request or capabilities."},"method":{"type":"string","description":"Raw LSP method for request."},"params":{"type":"object","description":"Raw JSON-RPC params for request."},"apply":{"type":"boolean","description":"Apply a returned rename/code-action WorkspaceEdit. Only files inside the workspace are accepted."},"timeout_ms":{"type":"integer","minimum":1,"maximum":120000,"description":"Request timeout in milliseconds (default 30000, maximum 120000)."},"max":{"type":"integer","minimum":1,"maximum":50},"run_cli":{"type":"boolean","description":"Run configured CLI linters for diagnostics (default true)."}},"required":["action"]}`

func (t *LSPTool) Name() string { return "lsp" }
func (t *LSPTool) Description() string {
	return "Query configured language servers and linters for diagnostics, navigation, refactors, symbols, capabilities, and raw LSP requests."
}
func (t *LSPTool) Schema() json.RawMessage { return json.RawMessage(lspSchema) }

func (t *LSPTool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var args lspArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid lsp args: %w", err)
	}
	args.Action = strings.ToLower(strings.TrimSpace(args.Action))
	if args.Action == "" {
		return core.ToolResult{}, fmt.Errorf("action is required")
	}
	if t.Manager == nil {
		return core.ToolResult{}, fmt.Errorf("lsp manager is unavailable")
	}
	cwd := t.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return core.ToolResult{}, err
	}
	path := ""
	if args.Path != "" && args.Path != "*" {
		path = resolvePath(absCWD, args.Path)
		if t.Sandbox != nil {
			if err := t.Sandbox.CheckPath(path); err != nil {
				return core.ToolResult{}, err
			}
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, lspToolTimeout(args.Timeout))
	defer cancel()
	var text string
	var details any
	var actionErr error
	switch args.Action {
	case "diagnostics":
		runCLI := true
		if args.RunCLI != nil {
			runCLI = *args.RunCLI
		}
		var diagnostics []lsp.Diagnostic
		if path != "" && hasGlobMeta(args.Path) {
			matches, globErr := filepath.Glob(path)
			if globErr != nil {
				actionErr = globErr
				break
			}
			for _, match := range matches {
				if t.Sandbox != nil {
					if checkErr := t.Sandbox.CheckPath(match); checkErr != nil {
						if actionErr == nil {
							actionErr = checkErr
						}
						continue
					}
				}
				values, diagErr := t.Manager.DiagnosticsWithOptions(runCtx, absCWD, match, runCLI)
				if actionErr == nil {
					actionErr = diagErr
				}
				diagnostics = append(diagnostics, values...)
			}
		} else {
			diagnostics, actionErr = t.Manager.DiagnosticsWithOptions(runCtx, absCWD, path, runCLI)
		}
		max := boundedMax(args.Max)
		text = lsp.SummarizeDiagnostics(diagnostics, absCWD, max)
		visible := diagnostics
		if len(visible) > max {
			visible = visible[:max]
		}
		details = map[string]any{"diagnostics": visible, "count": len(diagnostics), "truncated": len(visible) < len(diagnostics), "run_cli": runCLI}
	case "status":
		var statuses []lsp.ServerStatus
		statuses, actionErr = t.Manager.Status(absCWD, path)
		text = prettyJSON(statuses)
		details = statuses
	case "reload":
		actionErr = t.Manager.Reload(absCWD)
		text = "Reloaded LSP and linter processes and configuration."
	case "capabilities":
		var capabilities map[string]json.RawMessage
		capabilities, actionErr = t.Manager.Capabilities(runCtx, absCWD, path, args.Server)
		text = prettyJSON(capabilities)
		details = capabilities
	case "request":
		text, details, actionErr = t.executeRawRequest(runCtx, absCWD, path, args)
	case "definition", "references", "hover", "symbols", "rename", "code_actions", "type_definition", "implementation":
		text, details, actionErr = t.executeLanguageAction(runCtx, absCWD, path, args)
	default:
		return core.ToolResult{}, fmt.Errorf("unknown lsp action %q", args.Action)
	}
	if actionErr != nil && text == "" {
		return core.ToolResult{}, actionErr
	}
	text = boundLSPText(text)
	result := core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: text}}, IsError: actionErr != nil, Details: details}
	if values, ok := details.(map[string]any); ok {
		if paths, ok := values["modified_paths"].([]string); ok {
			result.Context.Mutates = paths
		}
	}
	return result, nil
}

func (t *LSPTool) executeLanguageAction(ctx context.Context, cwd, path string, args lspArgs) (string, any, error) {
	if path == "" && (args.Action != "symbols" || args.Query == "") {
		return "", nil, fmt.Errorf("path is required for %s", args.Action)
	}
	if args.Action != "symbols" && (args.Line < 1 || args.Column < 1) {
		return "", nil, fmt.Errorf("line and column (1-based) are required for %s", args.Action)
	}
	position := lsp.Position{Line: args.Line - 1, Character: args.Column - 1}
	method := map[string]string{"definition": "textDocument/definition", "references": "textDocument/references", "hover": "textDocument/hover", "symbols": "textDocument/documentSymbol", "rename": "textDocument/rename", "code_actions": "textDocument/codeAction", "type_definition": "textDocument/typeDefinition", "implementation": "textDocument/implementation"}[args.Action]
	var params map[string]any
	if args.Action == "symbols" {
		if args.Query != "" {
			method, params = "workspace/symbol", map[string]any{"query": args.Query}
		} else {
			params = map[string]any{"textDocument": map[string]any{"uri": fileURI(path)}}
		}
	} else {
		params = map[string]any{"textDocument": map[string]any{"uri": fileURI(path)}, "position": position}
		if args.Action == "references" {
			params["context"] = map[string]any{"includeDeclaration": true}
		}
		if args.Action == "code_actions" {
			params = map[string]any{"textDocument": map[string]any{"uri": fileURI(path)}, "range": lsp.Range{Start: position, End: position}, "context": map[string]any{"diagnostics": []any{}}}
		}
		if args.Action == "rename" {
			if strings.TrimSpace(args.NewName) == "" {
				return "", nil, fmt.Errorf("new_name is required for rename")
			}
			params["newName"] = args.NewName
		}
	}
	responses := t.Manager.RequestAll(ctx, cwd, path, method, params)
	apply := args.Apply != nil && *args.Apply
	if args.Action == "rename" && args.Apply == nil {
		apply = true
	}
	text, applyCount, modifiedPaths, applyErr := formatResponses(cwd, responses, args.Action, apply, t.Manager)
	if applyCount > 0 {
		text += fmt.Sprintf("\nApplied %d workspace edit(s).", applyCount)
	}
	details := map[string]any{"action": args.Action, "responses": responses, "applied": applyCount, "modified_paths": modifiedPaths}
	if applyCount > 0 && t.LSPDiagnostics {
		result := core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: text}}, Details: details}
		attachMutationDiagnostics(ctx, cwd, modifiedPaths, t.Manager, &result)
		text = result.Content[0].(provider.TextBlock).Text
	}
	return text, details, applyErr
}

func (t *LSPTool) executeRawRequest(ctx context.Context, cwd, path string, args lspArgs) (string, any, error) {
	if strings.TrimSpace(args.Method) == "" {
		return "", nil, fmt.Errorf("method is required for request")
	}
	var params any = map[string]any{}
	if len(args.Params) > 0 && string(args.Params) != "null" {
		if err := json.Unmarshal(args.Params, &params); err != nil {
			return "", nil, fmt.Errorf("params must be JSON: %w", err)
		}
	}
	var responses []lsp.Response
	if path != "" {
		_ = t.Manager.Open(ctx, cwd, path)
	}
	if args.Server != "" {
		result, err := t.Manager.Request(ctx, cwd, args.Server, args.Method, params)
		responses = []lsp.Response{{Server: args.Server, Result: result, Error: err}}
	} else {
		responses = t.Manager.RequestAll(ctx, cwd, path, args.Method, params)
	}
	return formatRawResponses(responses), map[string]any{"method": args.Method, "responses": responses}, firstResponseError(responses)
}

func formatResponses(cwd string, responses []lsp.Response, action string, apply bool, manager *lsp.Manager) (string, int, []string, error) {
	var b strings.Builder
	applied := 0
	var modifiedPaths []string
	var firstErr error
	pendingEdits := make([]lsp.WorkspaceEdit, 0)
	for _, response := range responses {
		if response.Error != nil {
			fmt.Fprintf(&b, "%s: error: %s\n", response.Server, response.Error)
			if firstErr == nil {
				firstErr = response.Error
			}
			continue
		}
		fmt.Fprintf(&b, "%s:\n%s\n", response.Server, prettyJSON(response.Result))
		if apply && (action == "rename" || action == "code_actions") {
			pendingEdits = append(pendingEdits, workspaceEdits(response.Result, action)...)
		}
	}
	if len(pendingEdits) > 0 {
		modifiedPaths = workspaceEditPaths(cwd, pendingEdits)
		if err := manager.ApplyEdits(cwd, pendingEdits); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			fmt.Fprintf(&b, "apply: error: %s\n", err)
		} else {
			applied = len(pendingEdits)
		}
	}
	if b.Len() == 0 {
		return "No results.", applied, modifiedPaths, firstErr
	}
	return b.String(), applied, modifiedPaths, firstErr
}

func workspaceEditPaths(cwd string, edits []lsp.WorkspaceEdit) []string {
	seen := make(map[string]bool)
	var paths []string
	add := func(uri string) {
		path, err := lsp.URIToPath(uri)
		if err != nil {
			return
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		path = filepath.Clean(path)
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	for _, edit := range edits {
		for uri := range edit.Changes {
			add(uri)
		}
		for _, raw := range edit.DocumentChanges {
			var change struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			if json.Unmarshal(raw, &change) == nil {
				add(change.TextDocument.URI)
			}
		}
	}
	return paths
}

func workspaceEdits(raw json.RawMessage, action string) []lsp.WorkspaceEdit {
	if action == "rename" {
		var edit lsp.WorkspaceEdit
		if json.Unmarshal(raw, &edit) == nil && (len(edit.Changes) > 0 || len(edit.DocumentChanges) > 0) {
			return []lsp.WorkspaceEdit{edit}
		}
		return nil
	}
	var actions []struct {
		Edit lsp.WorkspaceEdit `json:"edit"`
	}
	if json.Unmarshal(raw, &actions) != nil {
		return nil
	}
	out := make([]lsp.WorkspaceEdit, 0, len(actions))
	for _, action := range actions {
		if len(action.Edit.Changes) > 0 || len(action.Edit.DocumentChanges) > 0 {
			out = append(out, action.Edit)
		}
	}
	return out
}

func formatRawResponses(responses []lsp.Response) string {
	var b strings.Builder
	for _, response := range responses {
		if response.Error != nil {
			fmt.Fprintf(&b, "%s: error: %s\n", response.Server, response.Error)
			continue
		}
		fmt.Fprintf(&b, "%s:\n%s\n", response.Server, prettyJSON(response.Result))
	}
	if b.Len() == 0 {
		return "No results."
	}
	return b.String()
}

func firstResponseError(responses []lsp.Response) error {
	for _, response := range responses {
		if response.Error != nil {
			return response.Error
		}
	}
	return nil
}
func fileURI(path string) string { return lsp.URIForPath(path) }
func prettyJSON(raw any) string {
	var data []byte
	switch value := raw.(type) {
	case json.RawMessage:
		data = value
	default:
		data, _ = json.Marshal(value)
	}
	var value any
	if json.Unmarshal(data, &value) == nil {
		if formatted, err := json.MarshalIndent(value, "", "  "); err == nil {
			return string(formatted)
		}
	}
	return strings.TrimSpace(string(data))
}
func hasGlobMeta(value string) bool {
	return strings.ContainsAny(value, "*?[")
}

func lspToolTimeout(milliseconds int) time.Duration {
	if milliseconds <= 0 {
		return defaultLSPTimeout
	}
	if milliseconds > maxLSPTimeoutMillis {
		return maxLSPTimeout
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func boundedMax(value int) int {
	if value <= 0 || value > lsp.DefaultDiagnosticCap {
		return lsp.DefaultDiagnosticCap
	}
	return value
}
func boundLSPText(value string) string {
	if len(value) <= maxLSPToolOutput {
		return value
	}
	suffix := fmt.Sprintf("\n... [truncated at %d bytes]", maxLSPToolOutput)
	available := maxLSPToolOutput - len(suffix)
	cut := value[:available]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + suffix
}
