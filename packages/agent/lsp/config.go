// Package lsp implements a small, dependency-free LSP and linter backend.
package lsp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/bnema/zut/packages/agent/internal/execpath"
)

// ServerConfig describes either a stdio LSP server or a command-line linter.
// Command is resolved relative to the workspace before it is started.
type ServerConfig struct {
	ID                    string            `json:"-"`
	Kind                  string            `json:"kind,omitempty"` // "lsp" or "cli"
	Command               string            `json:"command"`
	Args                  []string          `json:"args,omitempty"`
	FileTypes             []string          `json:"fileTypes,omitempty"`
	LanguageID            string            `json:"languageId,omitempty"`
	RootMarkers           []string          `json:"rootMarkers,omitempty"`
	IsLinter              bool              `json:"isLinter,omitempty"`
	Disabled              bool              `json:"disabled,omitempty"`
	Parser                string            `json:"parser,omitempty"`
	Mode                  string            `json:"mode,omitempty"` // files or workspace for CLI linters
	RunOnStartup          bool              `json:"runOnStartup,omitempty"`
	RunOnChange           bool              `json:"runOnChange,omitempty"`
	Description           string            `json:"description,omitempty"`
	Env                   map[string]string `json:"env,omitempty"`
	Settings              map[string]any    `json:"settings,omitempty"`
	InitializationOptions any               `json:"initializationOptions,omitempty"`
}

// Config is the merged global/project LSP configuration.
type Config struct {
	Servers    map[string]ServerConfig
	AutoDetect bool
	// Explicit contains servers named or defined by a config file. When
	// auto-detection is disabled, only these configured providers remain
	// eligible, matching the providers/autoDetect configuration behavior.
	Explicit map[string]bool
	Files    []string
}

// BuiltinServers returns the built-in language servers. The slice is a copy
// and may be modified by callers.
func BuiltinServers() []ServerConfig {
	return []ServerConfig{
		{ID: "gopls", Kind: "lsp", Command: "gopls", FileTypes: []string{"go", ".go"}, RootMarkers: []string{"go.work", "go.mod"}, Description: "Go language server"},
		{ID: "rust-analyzer", Kind: "lsp", Command: "rust-analyzer", FileTypes: []string{"rust", "rs", ".rs"}, RootMarkers: []string{"Cargo.toml"}, Description: "Rust language server"},
		{ID: "typescript-language-server", Kind: "lsp", Command: "typescript-language-server", Args: []string{"--stdio"}, FileTypes: []string{"typescript", "typescriptreact", "javascript", "javascriptreact", "ts", "tsx", "js", "jsx", ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs"}, RootMarkers: []string{"tsconfig.json", "jsconfig.json", "package.json"}, Description: "TypeScript and JavaScript language server"},
		{ID: "pyright-langserver", Kind: "lsp", Command: "pyright-langserver", Args: []string{"--stdio"}, FileTypes: []string{"python", "py", ".py"}, RootMarkers: []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt"}, Description: "Python language server"},
		{ID: "clangd", Kind: "lsp", Command: "clangd", FileTypes: []string{"c", "cpp", "c++", "h", "hpp", "cc", "cpp", "cxx", ".c", ".h", ".cc", ".cpp", ".cxx", ".hpp"}, RootMarkers: []string{"compile_commands.json", "compile_flags.txt", "CMakeLists.txt"}, Description: "C and C++ language server"},
		{ID: "yaml-language-server", Kind: "lsp", Command: "yaml-language-server", Args: []string{"--stdio"}, FileTypes: []string{"yaml", "yml", ".yaml", ".yml"}, Description: "YAML language server"},
		{ID: "vscode-json-language-server", Kind: "lsp", Command: "vscode-json-language-server", Args: []string{"--stdio"}, FileTypes: []string{"json", "jsonc", ".json", ".jsonc"}, Description: "JSON language server"},
		{ID: "bash-language-server", Kind: "lsp", Command: "bash-language-server", Args: []string{"start"}, FileTypes: []string{"shellscript", "bash", "sh", "zsh", ".sh", ".bash", ".zsh"}, RootMarkers: []string{".shellcheckrc"}, Description: "Bash language server"},
		{ID: "eslint", Kind: "cli", Command: "eslint", Args: []string{"--format", "json"}, FileTypes: []string{"javascript", "javascriptreact", "typescript", "typescriptreact", ".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts"}, RootMarkers: []string{"eslint.config.js", "eslint.config.cjs", "eslint.config.mjs", "eslint.config.ts", ".eslintrc", ".eslintrc.json", ".eslintrc.js", ".eslintrc.cjs", ".eslintrc.yaml", ".eslintrc.yml"}, Parser: "eslint-json", Mode: "files", Description: "ESLint diagnostics"},
		{ID: "golangci-lint", Kind: "cli", Command: "golangci-lint", Args: []string{"run", "--output.json.path", "stdout"}, FileTypes: []string{"go", ".go"}, RootMarkers: []string{".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json"}, Parser: "golangci-lint-json", Mode: "workspace", Description: "golangci-lint diagnostics"},
		{ID: "ruff", Kind: "cli", Command: "ruff", Args: []string{"check", "--output-format", "json"}, FileTypes: []string{"python", "py", ".py"}, RootMarkers: []string{"ruff.toml", ".ruff.toml", "pyproject.toml"}, Parser: "ruff-json", Mode: "files", Description: "Ruff diagnostics"},
	}
}

// LoadConfig reads the global and project lsp.json files. Project files are
// applied after the global file; within a project, lsp.json, .lsp.json, and
// .zut/lsp.json are applied in that order. Missing files are harmless.
// A supplied env selects session-local home paths and requires an absolute cwd.
func LoadConfig(cwd string, env ...string) (Config, error) {
	if env != nil && !filepath.IsAbs(cwd) {
		return Config{}, fmt.Errorf("session workspace must be absolute: %q", cwd)
	}
	cwd, err := absDir(cwd)
	if err != nil {
		return Config{}, err
	}
	var files []string
	if home := zutHome(env...); home != "" {
		files = append(files, filepath.Join(home, "lsp.json"))
	}
	files = append(files,
		filepath.Join(cwd, ".pi", "lsp.json"),
		filepath.Join(cwd, ".omp", "lsp.json"),
		filepath.Join(cwd, "lsp.json"),
		filepath.Join(cwd, ".lsp.json"),
		filepath.Join(cwd, ".zut", "lsp.json"),
	)

	objects := make(map[string]map[string]json.RawMessage)
	explicit := make(map[string]bool)
	autoDetect := true
	for _, path := range files {
		entries, exists, data, err := readConfigFile(path)
		if err != nil {
			return Config{}, err
		}
		if len(data) > 0 {
			var metadata struct {
				AutoDetect *bool `json:"autoDetect"`
			}
			if json.Unmarshal(data, &metadata) == nil && metadata.AutoDetect != nil {
				autoDetect = *metadata.AutoDetect
			}
		}
		if !exists {
			continue
		}
		for id := range entries {
			explicit[id] = true
		}
		for id := range providerIDs(data) {
			explicit[id] = true
		}
		for id, entry := range entries {
			if old := objects[id]; old != nil {
				objects[id] = mergeRawObjects(old, entry)
			} else {
				objects[id] = cloneRawObject(entry)
			}
		}
	}

	merged := make(map[string]ServerConfig)
	for _, server := range BuiltinServers() {
		merged[server.ID] = server
	}
	for id, raw := range objects {
		decodeRaw := raw
		_, hasBuiltin := merged[id]
		if base, ok := merged[id]; ok {
			// Decode the already-merged JSON over the built-in by marshaling the
			// built-in first. This retains fields that were omitted in the file.
			baseRaw, _ := json.Marshal(base)
			baseMap := map[string]json.RawMessage{}
			_ = json.Unmarshal(baseRaw, &baseMap)
			decodeRaw = mergeRawObjects(baseMap, raw)
		}
		server, err := decodeServer(id, decodeRaw)
		if err != nil {
			return Config{}, fmt.Errorf("lsp server %q: %w", id, err)
		}
		// A parser/mode is an explicit CLI declaration even when the
		// overridden built-in was an LSP. This makes {"parser":"sarif"}
		// sufficient for a linter override.
		if hasBuiltin {
			if _, hasKind := raw["kind"]; !hasKind {
				if _, hasParser := raw["parser"]; hasParser {
					server.Kind = "cli"
				}
			}
		}
		merged[id] = server
	}
	for id, server := range merged {
		if server.ID == "" {
			server.ID = id
			merged[id] = server
		}
	}
	return Config{Servers: merged, AutoDetect: autoDetect, Explicit: explicit, Files: files}, nil
}

// LoadServers is a convenience for callers that only need auto-detected
// server configurations.
func LoadServers(cwd string) ([]ServerConfig, error) {
	config, err := LoadConfig(cwd)
	if err != nil {
		return nil, err
	}
	return config.EffectiveServers(cwd), nil
}

// EffectiveServers returns enabled servers which are relevant to any file in
// the workspace. It is useful for status UIs; Manager uses ApplicableServers
// for a particular file.
func (c Config) EffectiveServers(cwd string) []ServerConfig {
	var out []ServerConfig
	for _, server := range c.Servers {
		if server.Disabled || (!c.AutoDetect && !c.Explicit[server.ID]) {
			continue
		}
		if len(server.FileTypes) == 0 && len(server.RootMarkers) == 0 {
			out = append(out, server)
			continue
		}
		if hasRootMarker(cwd, server.RootMarkers) || hasMatchingFile(cwd, server.FileTypes) {
			out = append(out, server)
		}
	}
	sortServers(out)
	return out
}

// ApplicableServers selects servers for path. An empty path means the cwd.
func (c Config) ApplicableServers(cwd, path string) []ServerConfig {
	var out []ServerConfig
	for _, server := range c.Servers {
		if server.Disabled || (!c.AutoDetect && !c.Explicit[server.ID]) || !serverMatchesPath(cwd, path, server) {
			continue
		}
		out = append(out, server)
	}
	sortServers(out)
	return out
}

func serverMatchesPath(cwd, path string, server ServerConfig) bool {
	if len(server.FileTypes) == 0 {
		return len(server.RootMarkers) == 0 || hasRootMarker(cwd, server.RootMarkers)
	}
	if path != "" {
		if len(server.RootMarkers) > 0 && !hasRootMarker(cwd, server.RootMarkers) {
			return false
		}
		return fileTypeMatches(path, server.FileTypes)
	}
	return hasRootMarker(cwd, server.RootMarkers) || hasMatchingFile(cwd, server.FileTypes)
}

func hasMatchingFile(cwd string, types []string) bool {
	normalizedTypes := normalizeFileTypes(types)
	var found bool
	_ = filepath.WalkDir(cwd, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if found {
			return filepath.SkipAll
		}
		if entry.IsDir() {
			if path != cwd && (entry.Name() == ".git" || entry.Name() == "node_modules" || entry.Name() == ".venv") {
				return filepath.SkipDir
			}
			return nil
		}
		found = fileTypeMatchesNormalized(path, normalizedTypes)
		if found {
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func fileTypeMatches(path string, types []string) bool {
	return fileTypeMatchesNormalized(path, normalizeFileTypes(types))
}

func normalizeFileTypes(types []string) []string {
	normalized := make([]string, 0, len(types))
	for _, typ := range types {
		normalized = append(normalized, strings.ToLower(strings.TrimSpace(typ)))
	}
	return normalized
}

func fileTypeMatchesNormalized(path string, types []string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	base := strings.ToLower(filepath.Base(path))
	lang := languageForExtension(ext)
	for _, typ := range types {
		if typ == ext || typ == "."+strings.TrimPrefix(ext, ".") || typ == base || typ == lang || strings.TrimPrefix(typ, ".") == strings.TrimPrefix(ext, ".") {
			return true
		}
		if strings.HasPrefix(typ, "*.") && strings.HasSuffix(base, strings.TrimPrefix(typ, "*")) {
			return true
		}
	}
	return false
}

func hasRootMarker(cwd string, markers []string) bool {
	if len(markers) == 0 {
		return false
	}
	cwd, _ = absDir(cwd)
	for dir := cwd; dir != ""; dir = filepath.Dir(dir) {
		for _, marker := range markers {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return true
			}
		}
		next := filepath.Dir(dir)
		if next == dir {
			break
		}
	}
	return false
}

// FindRoot resolves a server's workspace root by walking up from cwd and
// choosing the nearest directory containing one of its root markers.
func FindRoot(cwd string, markers []string) string {
	cwd, err := absDir(cwd)
	if err != nil {
		return cwd
	}
	for dir := cwd; dir != ""; dir = filepath.Dir(dir) {
		for _, marker := range markers {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		next := filepath.Dir(dir)
		if next == dir {
			break
		}
	}
	return cwd
}

func sortServers(servers []ServerConfig) {
	rank := func(server ServerConfig) int {
		if server.Kind == "lsp" && !server.IsLinter {
			return 0
		}
		if server.Kind == "lsp" {
			return 1
		}
		return 2
	}
	for i := 1; i < len(servers); i++ {
		for j := i; j > 0; j-- {
			left, right := servers[j-1], servers[j]
			if rank(left) < rank(right) || (rank(left) == rank(right) && left.ID <= right.ID) {
				break
			}
			servers[j-1], servers[j] = right, left
		}
	}
}

func absDir(cwd string) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		cwd, _ = os.Getwd()
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err == nil && !info.IsDir() {
		return "", fmt.Errorf("workspace %q is not a directory", cwd)
	}
	return filepath.Clean(abs), nil
}

func zutHome(env ...string) string {
	lookup := os.Getenv
	home, _ := os.UserHomeDir()
	if env != nil {
		lookup = func(k string) string { return execpath.LookupEnv(env, k) }
		home = lookup("HOME")
		if runtime.GOOS == "windows" {
			home = lookup("USERPROFILE")
		}
	}
	if value := strings.TrimSpace(lookup("ZUT_HOME")); value != "" {
		return value
	}
	if value := strings.TrimSpace(lookup("XDG_STATE_HOME")); value != "" {
		return filepath.Join(value, "zut")
	}
	switch runtime.GOOS {
	case "darwin":
		if home == "" {
			return ""
		}
		return filepath.Join(home, "Library", "Application Support", "zut")
	case "windows":
		if value := strings.TrimSpace(lookup("LOCALAPPDATA")); value != "" {
			return filepath.Join(value, "zut")
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "zut")
}

func providerIDs(data []byte) map[string]struct{} {
	ids := make(map[string]struct{})
	var top struct {
		Providers []json.RawMessage `json:"providers"`
	}
	if json.Unmarshal(data, &top) != nil {
		return ids
	}
	for _, raw := range top.Providers {
		var id string
		if json.Unmarshal(raw, &id) == nil {
			if id = strings.TrimSpace(id); id != "" {
				ids[id] = struct{}{}
			}
			continue
		}
		var object struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &object) == nil {
			if id = strings.TrimSpace(object.ID); id != "" {
				ids[id] = struct{}{}
			}
		}
	}
	return ids
}

func readConfigFile(path string) (map[string]map[string]json.RawMessage, bool, []byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil, nil
	}
	if err != nil {
		return nil, true, nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, true, data, fmt.Errorf("read %s: %w", path, err)
	}
	root := top
	if raw, ok := top["providers"]; ok {
		// A providers array may contain compact string entries. Preserve
		// them (built-ins are already in the default registry)
		// and turn object entries into the map shape used internally.
		var providers []json.RawMessage
		if err := json.Unmarshal(raw, &providers); err != nil {
			return nil, true, data, fmt.Errorf("%s.providers must be an array: %w", path, err)
		}
		root = make(map[string]json.RawMessage)
		for index, provider := range providers {
			var id string
			if json.Unmarshal(provider, &id) == nil {
				continue
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(provider, &object); err != nil {
				return nil, true, data, fmt.Errorf("%s.providers[%d] must be a string or object: %w", path, index, err)
			}
			idRaw, ok := object["id"]
			if !ok || json.Unmarshal(idRaw, &id) != nil || strings.TrimSpace(id) == "" {
				return nil, true, data, fmt.Errorf("%s.providers[%d] is missing id", path, index)
			}
			id = strings.TrimSpace(id)
			delete(object, "id")
			flattenProviderObject(object)
			root[id] = mustJSON(object)
		}
	} else {
		wrappedRoot := make(map[string]json.RawMessage)
		wrapped := false
		for _, key := range []string{"servers", "lspServers", "linters"} {
			if raw, ok := top[key]; ok {
				var section map[string]json.RawMessage
				if err := json.Unmarshal(raw, &section); err != nil {
					return nil, true, data, fmt.Errorf("%s.%s must be an object: %w", path, key, err)
				}
				wrapped = true
				for id, value := range section {
					wrappedRoot[id] = value
				}
			}
		}
		if wrapped {
			root = wrappedRoot
		}
	}
	out := make(map[string]map[string]json.RawMessage)
	for id, raw := range root {
		if id == "$schema" || id == "servers" || id == "lspServers" || id == "linters" || id == "providers" || id == "autoDetect" || id == "version" || id == "debug" || id == "status" || id == "lifecycle" || id == "excludePaths" || id == "idleTimeoutMs" {
			continue
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			// A string is a convenient shorthand for a command.
			var command string
			if json.Unmarshal(raw, &command) == nil {
				object = map[string]json.RawMessage{"command": json.RawMessage(strconvQuote(command))}
			} else {
				return nil, true, data, fmt.Errorf("%s.%s must be an object", path, id)
			}
		}
		out[id] = object
	}
	return out, true, data, nil
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// flattenProviderObject adapts nested provider shapes to the flat
// ServerConfig fields used by zut. Unknown provider metadata remains
// harmless JSON and is ignored by decodeServer.
func flattenProviderObject(object map[string]json.RawMessage) {
	if _, exists := object["command"]; !exists {
		var candidates []struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		}
		if raw, ok := object["commandCandidates"]; ok && json.Unmarshal(raw, &candidates) == nil && len(candidates) > 0 && candidates[0].Command != "" {
			object["command"] = mustJSON(candidates[0].Command)
			if len(candidates[0].Args) > 0 {
				object["args"] = mustJSON(candidates[0].Args)
			}
		}
	}
	if _, exists := object["kind"]; !exists {
		if _, ok := object["cli"]; ok {
			object["kind"] = mustJSON("cli")
		} else if _, ok := object["lsp"]; ok {
			object["kind"] = mustJSON("lsp")
		}
	}
	if _, exists := object["fileTypes"]; !exists {
		var selectors []struct {
			LanguageID string   `json:"languageId"`
			Extensions []string `json:"extensions"`
			Filenames  []string `json:"filenames"`
		}
		if raw, ok := object["selectors"]; ok && json.Unmarshal(raw, &selectors) == nil {
			var fileTypes []string
			for _, selector := range selectors {
				if selector.LanguageID != "" {
					fileTypes = append(fileTypes, selector.LanguageID)
				}
				fileTypes = append(fileTypes, selector.Extensions...)
				fileTypes = append(fileTypes, selector.Filenames...)
			}
			if len(fileTypes) > 0 {
				object["fileTypes"] = mustJSON(uniqueStrings(fileTypes))
			}
		}
		if _, exists := object["fileTypes"]; !exists {
			var detect struct {
				FileExtensions []string `json:"fileExtensionsAny"`
			}
			if raw, ok := object["detect"]; ok && json.Unmarshal(raw, &detect) == nil && len(detect.FileExtensions) > 0 {
				object["fileTypes"] = mustJSON(uniqueStrings(detect.FileExtensions))
			}
		}
	}
	if _, exists := object["rootMarkers"]; !exists {
		var detect struct {
			RepoFiles   []string `json:"repoFilesAny"`
			ConfigFiles []string `json:"configFilesAny"`
		}
		if raw, ok := object["detect"]; ok && json.Unmarshal(raw, &detect) == nil {
			markers := append(append([]string{}, detect.RepoFiles...), detect.ConfigFiles...)
			if len(markers) > 0 {
				object["rootMarkers"] = mustJSON(uniqueStrings(markers))
			}
		}
		if _, exists := object["rootMarkers"]; !exists {
			if raw, ok := object["configMarkers"]; ok {
				object["rootMarkers"] = append(json.RawMessage(nil), raw...)
			}
		}
	}
	if raw, ok := object["lsp"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil {
			if _, exists := object["initializationOptions"]; !exists {
				if value, ok := nested["initializationOptions"]; ok {
					object["initializationOptions"] = append(json.RawMessage(nil), value...)
				}
			}
		}
	}
	if raw, ok := object["cli"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil {
			for _, key := range []string{"parser", "mode", "runOnStartup", "runOnChange"} {
				if _, exists := object[key]; exists {
					continue
				}
				if value, ok := nested[key]; ok {
					object[key] = append(json.RawMessage(nil), value...)
				}
			}
		}
	}
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func decodeServer(id string, raw map[string]json.RawMessage) (ServerConfig, error) {
	if _, exists := raw["initializationOptions"]; !exists {
		if value, ok := raw["initOptions"]; ok {
			raw["initializationOptions"] = append(json.RawMessage(nil), value...)
		}
	}
	if command, ok := raw["command"]; ok {
		var list []string
		if err := json.Unmarshal(command, &list); err == nil {
			if len(list) == 0 {
				return ServerConfig{}, fmt.Errorf("command array is empty")
			}
			raw["command"] = json.RawMessage(strconvQuote(list[0]))
			if len(list) > 1 {
				args, _ := json.Marshal(list[1:])
				if _, exists := raw["args"]; !exists {
					raw["args"] = args
				}
			}
		}
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return ServerConfig{}, err
	}
	var server ServerConfig
	if err := json.Unmarshal(data, &server); err != nil {
		return ServerConfig{}, err
	}
	server.ID = id
	if server.Kind == "" {
		if server.Parser != "" || server.Mode != "" {
			server.Kind = "cli"
		} else {
			server.Kind = "lsp"
		}
	}
	server.Kind = strings.ToLower(server.Kind)
	if server.Kind != "lsp" && server.Kind != "cli" {
		return ServerConfig{}, fmt.Errorf("kind must be lsp or cli")
	}
	if server.Kind == "cli" && server.Parser == "" {
		server.Parser = "generic"
	}
	return server, nil
}

func cloneRawObject(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in))
	for key, value := range in {
		out[key] = append(json.RawMessage(nil), value...)
	}
	return out
}

func mergeRawObjects(base, override map[string]json.RawMessage) map[string]json.RawMessage {
	out := cloneRawObject(base)
	for key, value := range override {
		var baseObject, overrideObject map[string]json.RawMessage
		if json.Unmarshal(out[key], &baseObject) == nil && json.Unmarshal(value, &overrideObject) == nil {
			out[key] = mustJSON(mergeRawObjects(baseObject, overrideObject))
			continue
		}
		out[key] = append(json.RawMessage(nil), value...)
	}
	return out
}

func mustJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func languageForExtension(ext string) string {
	switch strings.ToLower(ext) {
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".py":
		return "python"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	case ".c", ".h":
		return "c"
	case ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx":
		return "cpp"
	case ".yaml", ".yml":
		return "yaml"
	case ".json", ".jsonc":
		return "json"
	case ".sh", ".bash", ".zsh":
		return "shellscript"
	default:
		return ""
	}
}
