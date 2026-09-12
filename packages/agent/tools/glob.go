package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/ignore"
	"github.com/bnema/zut/packages/provider"
)

const (
	maxGlobInputBytes = 4 * 1024
	maxGlobMatches    = 500
	maxGlobDepth      = 24
)

// GlobTool finds files matching a pattern within a directory tree.
type GlobTool struct {
	CWD     string
	Sandbox *Sandbox
}

var _ core.Tool = (*GlobTool)(nil)

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Hidden  bool   `json:"hidden,omitempty"`
}

const globSchema = `{"type":"object","properties":{"pattern":{"type":"string","description":"Glob pattern to match files against (e.g. \"**/*.go\", \"*.json\", \"src/**/*.ts\")"},"path":{"type":"string","description":"Directory to search within, relative to CWD (defaults to \".\")"},"hidden":{"type":"boolean","description":"Whether to include hidden files and directories (default false)"}},"required":["pattern"],"additionalProperties":false}`

func (t *GlobTool) Name() string { return "glob" }

func (t *GlobTool) Description() string {
	return "Find files matching a glob pattern (e.g. \"**/*.go\", \"*.json\", \"src/**/*.ts\"). Honors .gitignore rules."
}

func (t *GlobTool) Schema() json.RawMessage { return json.RawMessage(globSchema) }

// Execute walks the requested directory and returns the files matching the
// pattern. The search root is resolved and checked before traversal so a
// denied path cannot be used to probe the filesystem.
func (t *GlobTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	args, err := parseGlobArgs(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}

	re, hasSlash, err := compileGlob(args.Pattern)
	if err != nil {
		return core.ToolResult{}, err
	}

	root := filepath.Clean(resolvePath(t.CWD, args.Path))
	if !filepath.IsAbs(root) {
		root, err = filepath.Abs(root)
		if err != nil {
			return core.ToolResult{}, err
		}
	}
	if err := t.Sandbox.CheckReadPath(root); err != nil {
		return core.ToolResult{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return core.ToolResult{}, err
	}
	if !info.IsDir() {
		shown := t.Sandbox.DisplayPath(root, args.Path)
		return core.ToolResult{}, fmt.Errorf("glob: %s is not a directory", shown)
	}

	// WalkDir does not follow a symlink used as its root. Resolve the
	// explicit search directory so a path that Stat identified as a
	// directory is actually traversed, then re-check the resolved target
	// before reading it.
	searchDir, err := filepath.EvalSymlinks(root)
	if err != nil {
		return core.ToolResult{}, err
	}
	if err := t.Sandbox.CheckReadPath(searchDir); err != nil {
		return core.ToolResult{}, err
	}

	stack, ignoreRoot, searchIgnored := t.globIgnoreStack(searchDir)
	if searchIgnored {
		// An explicitly requested root is always walked, even when an
		// ignore rule matches it: like ripgrep with an explicit path, the
		// user's narrowing wins over inherited rules. Nested ignore files
		// still apply inside the walk.
		stack, ignoreRoot = ignore.NewStack(searchDir), searchDir
	}
	rootSep := strings.Count(searchDir, string(os.PathSeparator))
	var pushed []string
	var matches []string
	truncated := false

	var walkErr error
	walkErr = filepath.WalkDir(searchDir, func(path string, d os.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == searchDir {
			return nil
		}

		rel, relErr := filepath.Rel(searchDir, path)
		if relErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)

		// Hidden entries are skipped unless explicitly requested.
		// .git is always skipped. Nested symlinks are never followed: a
		// symlink to a directory is reported by WalkDir as a non-directory,
		// so resolve it explicitly and skip directory targets instead of
		// listing them as file matches.
		if d.Type()&os.ModeSymlink != 0 {
			if target, evalErr := filepath.EvalSymlinks(path); evalErr == nil {
				if targetInfo, statErr := os.Stat(target); statErr == nil && targetInfo.IsDir() {
					return nil
				}
			}
		}
		if !args.Hidden {
			if strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		} else if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}

		ignoreRel, ignoreRelErr := filepath.Rel(ignoreRoot, path)
		if ignoreRelErr != nil {
			return nil
		}
		ignoreRelSlash := filepath.ToSlash(ignoreRel)

		// Ignore filtering: pop stack frames for directories no
		// longer in scope.
		dirSlash := relSlash
		if !d.IsDir() {
			if idx := strings.LastIndex(relSlash, "/"); idx >= 0 {
				dirSlash = relSlash[:idx]
			} else {
				dirSlash = ""
			}
		}
		for len(pushed) > 0 {
			top := pushed[len(pushed)-1]
			if top == dirSlash || strings.HasPrefix(dirSlash, top+"/") {
				break
			}
			pushed = pushed[:len(pushed)-1]
			stack.Pop()
		}

		if stack.Match(ignoreRelSlash, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			if strings.Count(path, string(os.PathSeparator))-rootSep >= maxGlobDepth {
				return filepath.SkipDir
			}
			stack.Push(path, ignoreRel)
			pushed = append(pushed, relSlash)
			return nil
		}

		var matched bool
		if hasSlash {
			matched = re.MatchString(relSlash)
		} else {
			matched = re.MatchString(d.Name())
		}
		if matched {
			matches = append(matches, t.globDisplayPath(root, args.Path, relSlash))
			if len(matches) >= maxGlobMatches {
				truncated = true
				return filepath.SkipAll
			}
		}
		return nil
	})

	if walkErr != nil && walkErr != filepath.SkipAll {
		return core.ToolResult{}, walkErr
	}

	sort.Strings(matches)

	var text string
	if len(matches) == 0 {
		text = "No files matched the pattern."
	} else {
		text = strings.Join(matches, "\n")
		if truncated {
			text += fmt.Sprintf("\n\n(Truncated: showing first %d matches)", maxGlobMatches)
		}
	}

	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: text}},
		Details: map[string]any{
			"matches":   len(matches),
			"truncated": truncated,
			"pattern":   args.Pattern,
		},
	}, nil
}

func parseGlobArgs(raw json.RawMessage) (globArgs, error) {
	if len(raw) == 0 || len(raw) > maxGlobInputBytes {
		return globArgs{}, fmt.Errorf("glob: invalid arguments")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return globArgs{}, fmt.Errorf("glob: invalid arguments")
	}
	for key := range fields {
		if key != "pattern" && key != "path" && key != "hidden" {
			return globArgs{}, fmt.Errorf("glob: unknown argument %q", key)
		}
	}
	var args globArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return globArgs{}, fmt.Errorf("glob: invalid arguments: %w", err)
	}
	if _, ok := fields["pattern"]; !ok || strings.TrimSpace(args.Pattern) == "" {
		return globArgs{}, fmt.Errorf("glob: pattern is required")
	}
	if !utf8.ValidString(args.Pattern) || !utf8.ValidString(args.Path) ||
		strings.IndexByte(args.Pattern, 0) >= 0 || strings.IndexByte(args.Path, 0) >= 0 {
		return globArgs{}, fmt.Errorf("glob: arguments contain invalid characters")
	}
	return args, nil
}

// globIgnoreStack builds an ignore stack rooted at CWD when the search path
// is below it, preloading the ignore files between CWD and the search root.
// This preserves inherited rules when the caller narrows a search with path.
func (t *GlobTool) globIgnoreStack(searchDir string) (*ignore.Stack, string, bool) {
	cwd := resolvePath(t.CWD, ".")
	cwd, err := filepath.EvalSymlinks(cwd)
	if err != nil || t.Sandbox.CheckReadPath(cwd) != nil {
		return ignore.NewStack(searchDir), searchDir, false
	}

	rel, err := filepath.Rel(cwd, searchDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return ignore.NewStack(searchDir), searchDir, false
	}

	stack := ignore.NewStack(cwd)
	if rel == "." {
		return stack, cwd, false
	}

	current := cwd
	var relParts []string
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		current = filepath.Join(current, part)
		relParts = append(relParts, part)
		currentRel := filepath.Join(relParts...)
		if stack.Match(filepath.ToSlash(currentRel), true) {
			return stack, cwd, true
		}
		stack.Push(current, currentRel)
	}
	return stack, cwd, false
}

func (t *GlobTool) globDisplayPath(searchDir, given, relSlash string) string {
	if given == "" || given == "." {
		return relSlash
	}
	// Prefer the path exactly as requested so a symlinked search root
	// keeps its name in results instead of the resolved target.
	if !filepath.IsAbs(given) {
		cleaned := filepath.ToSlash(filepath.Clean(given))
		if cleaned != "" && cleaned != "." {
			return strings.TrimSuffix(cleaned, "/") + "/" + relSlash
		}
		return relSlash
	}
	prefix := filepath.ToSlash(t.Sandbox.DisplayPath(searchDir, given))
	if prefix == "" || prefix == "." {
		return relSlash
	}
	return strings.TrimSuffix(prefix, "/") + "/" + relSlash
}

// compileGlob converts a slash-normalized glob pattern into a regular
// expression. It supports '*', '**', '?', and character classes '[...]'.
// Patterns containing a slash match against the path relative to the search
// root; patterns without one match against the file basename.
func compileGlob(pattern string) (*regexp.Regexp, bool, error) {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	pattern = strings.TrimPrefix(pattern, "./")
	if pattern == "" {
		return nil, false, fmt.Errorf("glob: empty pattern")
	}

	hasSlash := strings.Contains(pattern, "/")

	var sb strings.Builder
	sb.WriteString("^")
	i := 0
	for i < len(pattern) {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i += 2
				if i < len(pattern) && pattern[i] == '/' {
					i++
					sb.WriteString("(?:.*/)?")
				} else {
					sb.WriteString(".*")
				}
			} else {
				i++
				if hasSlash {
					sb.WriteString("[^/]*")
				} else {
					sb.WriteString(".*")
				}
			}
		case '?':
			i++
			if hasSlash {
				sb.WriteString("[^/]")
			} else {
				sb.WriteString(".")
			}
		case '[':
			j := i + 1
			negated := false
			if j < len(pattern) && (pattern[j] == '^' || pattern[j] == '!') {
				negated = pattern[j] == '!'
				j++
			}
			if j < len(pattern) && pattern[j] == ']' {
				j++
			}
			for j < len(pattern) && pattern[j] != ']' {
				j++
			}
			if j < len(pattern) {
				class := pattern[i+1 : j+1]
				if negated {
					class = "^" + class[1:]
				}
				sb.WriteString("[" + class)
				i = j + 1
			} else {
				return nil, false, fmt.Errorf("glob: invalid pattern %q: unterminated character class", pattern)
			}
		case '.', '+', '(', ')', '^', '$', '|', '\\', '{', '}', ',':
			sb.WriteByte('\\')
			sb.WriteByte(c)
			i++
		default:
			sb.WriteByte(c)
			i++
		}
	}
	sb.WriteString("$")

	re, err := regexp.Compile(sb.String())
	if err != nil {
		return nil, false, fmt.Errorf("glob: invalid pattern %q: %w", pattern, err)
	}
	return re, hasSlash, nil
}
