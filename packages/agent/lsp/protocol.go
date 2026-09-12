package lsp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// Position and Range use the zero-based line and UTF-16 character indexing
// defined by LSP.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// WorkspaceEdit is the safe subset of a workspace edit understood by zut.
// Resource operations in DocumentChanges are deliberately rejected.
type WorkspaceEdit struct {
	Changes           map[string][]TextEdit `json:"changes,omitempty"`
	DocumentChanges   []json.RawMessage     `json:"documentChanges,omitempty"`
	ChangeAnnotations json.RawMessage       `json:"changeAnnotations,omitempty"`
}

// Diagnostic is the normalized diagnostic representation shared by LSP and
// CLI linters. Path is absolute when it came from a workspace-backed source.
type Diagnostic struct {
	Path     string `json:"path"`
	Range    Range  `json:"range"`
	Severity string `json:"severity"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message"`
	Source   string `json:"source,omitempty"`
	Server   string `json:"server,omitempty"`
	Clear    bool   `json:"-"`
}

func (d *Diagnostic) UnmarshalJSON(data []byte) error {
	var raw struct {
		Range    Range           `json:"range"`
		Severity json.RawMessage `json:"severity"`
		Code     json.RawMessage `json:"code"`
		Message  string          `json:"message"`
		Source   string          `json:"source"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	d.Range, d.Message, d.Source = raw.Range, raw.Message, raw.Source
	d.Severity = severityFromJSON(raw.Severity)
	d.Code = stringFromJSON(raw.Code)
	return nil
}

func severityFromJSON(raw json.RawMessage) string {
	var n int
	if json.Unmarshal(raw, &n) == nil {
		switch n {
		case 1:
			return "error"
		case 2:
			return "warning"
		case 3:
			return "info"
		case 4:
			return "hint"
		}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch strings.ToLower(s) {
		case "error", "err", "fatal":
			return "error"
		case "warning", "warn":
			return "warning"
		case "hint":
			return "hint"
		default:
			return "info"
		}
	}
	return "info"
}

func stringFromJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return strings.Trim(string(raw), `"`)
}

// URIForPath returns the escaped file URI used by LSP document params.
func URIForPath(path string) string {
	uri, _ := pathToURI(path)
	return uri
}

func pathToURI(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// url.URL escapes spaces and non-ASCII while retaining path separators.
	uriPath := filepath.ToSlash(abs)
	if runtime.GOOS == "windows" && strings.HasPrefix(uriPath, "//") {
		unc := strings.TrimPrefix(uriPath, "//")
		if host, rest, ok := strings.Cut(unc, "/"); ok {
			return (&url.URL{Scheme: "file", Host: host, Path: "/" + rest}).String(), nil
		}
	}
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := &url.URL{Scheme: "file", Path: uriPath}
	return u.String(), nil
}

// URIToPath converts an LSP document URI to a platform-native path.
func URIToPath(uri string) (string, error) {
	return uriToPath(uri)
}

func uriToPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	if u.Scheme != "" && !strings.EqualFold(u.Scheme, "file") {
		return "", fmt.Errorf("unsupported document URI scheme %q", u.Scheme)
	}
	path := u.Path
	if u.Host != "" && u.Host != "localhost" {
		path = "//" + u.Host + path
	}
	if path == "" {
		return "", fmt.Errorf("document URI has no path")
	}
	if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' { // file:///C:/...
		path = path[1:]
	}
	return filepath.Clean(filepath.FromSlash(path)), nil
}
