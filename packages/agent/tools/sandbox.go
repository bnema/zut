package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Sandbox guards tool access to the filesystem and shell. When Locked
// is true (1), file tools refuse paths outside Root and bash runs with
// a restricted environment.
//
// The value is designed to be shared across tool instances (by pointer).
// Enable/Disable are atomic so they can be toggled from the TUI.
type Sandbox struct {
	Root        string
	Permissions *PermissionSet
	locked      atomic.Bool
}

// NewSandbox returns a Sandbox rooted at cwd. It starts unlocked.
func NewSandbox(root string) *Sandbox {
	s := &Sandbox{Root: root}
	return s
}

// Lock enables sandboxing.
func (s *Sandbox) Lock() { s.locked.Store(true) }

// Unlock disables sandboxing.
func (s *Sandbox) Unlock() { s.locked.Store(false) }

// Locked reports whether the sandbox is enforcing limits.
func (s *Sandbox) Locked() bool { return s != nil && s.locked.Load() }

// CheckPath verifies that path resolves inside the sandbox root.
// Returns an error describing the violation if not. No-op when unlocked.
// Callers should pass an already-absolute path (use resolvePath() first).
func (s *Sandbox) CheckPath(path string) error {
	if !s.Locked() {
		return nil
	}
	rootAbs, err := canonical(s.Root)
	if err != nil {
		return fmt.Errorf("sandbox root: %w", err)
	}
	// Resolve the target to an absolute path. Walk up until we find an
	// existing parent so symlinks inside nonexistent dirs are still caught.
	target, err := canonicalOrParent(path)
	if err != nil {
		return fmt.Errorf("sandbox path: %w", err)
	}
	if !isUnder(rootAbs, target) {
		return denied("jailed: path %q is outside sandbox root %q (use /unjail to disable)", path, s.Root)
	}
	return nil
}

// DisplayPath returns the path the model should see in tool results
// and error messages. When jailed, an absolute path inside the
// sandbox root is rewritten relative to that root ("./pkg/foo.go"),
// which keeps absolute paths out of the context window so the model
// is nudged toward relative paths instead of trying to escape the
// jail (see issue #39). Paths outside root, unjailed sessions, and
// already-relative inputs are returned unchanged.
//
// abs should be the resolved absolute path (resolvePath output);
// given is the path exactly as the model supplied it, used as the
// fallback when no better relative form is available.
func (s *Sandbox) DisplayPath(abs, given string) string {
	if !s.Locked() {
		return given
	}
	rootAbs, err := canonical(s.Root)
	if err != nil {
		return given
	}
	target, err := canonicalOrParent(abs)
	if err != nil {
		return given
	}
	if !isUnder(rootAbs, target) {
		return given
	}
	rel, err := filepath.Rel(rootAbs, target)
	if err != nil || rel == "" || strings.HasPrefix(rel, "..") {
		return given
	}
	if rel == "." {
		return "."
	}
	return "./" + filepath.ToSlash(rel)
}

// CheckCommand applies a lightweight sanity check to a bash command
// when jailed. We cannot fully sandbox a shell, but we can reject the
// most obvious escapes so the model does not accidentally touch files
// outside root via shell arguments or redirections.
func (s *Sandbox) CheckCommand(cmd string) error {
	if !s.Locked() {
		return nil
	}
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return nil
	}
	// Reject obvious destructive roots.
	banned := []string{
		"rm -rf /", "rm -rf ~", "rm -rf $HOME",
		"sudo ", "su ",
		"chmod -R ", "chown -R ",
		"mkfs", "dd if=", "dd of=/",
	}
	lower := strings.ToLower(cmd)
	for _, b := range banned {
		if strings.Contains(lower, strings.ToLower(b)) {
			return denied("jailed: command contains banned pattern %q (use /unjail to disable)", b)
		}
	}
	// Heuristic: reject a leading `cd` that tries to move the shell out
	// of the sandbox. We only reject when the target actually resolves
	// outside root; a `cd` into a subdirectory of root (even spelled as
	// an absolute path) is allowed, because the model frequently does
	// `cd /abs/path/inside/root && build` and blanket-rejecting that
	// wastes turns and nudges the model toward trying to break out.
	// Note this only catches simple cases; a determined adversary can
	// still escape. This is a speed bump for the model, not a security
	// boundary.
	first := strings.TrimSpace(strings.SplitN(cmd, ";", 2)[0])
	first = strings.TrimSpace(strings.SplitN(first, "&&", 2)[0])
	if target, ok := cdTarget(first); ok {
		if err := s.checkCDTarget(target); err != nil {
			return err
		}
	}
	if err := s.checkCommandPaths(cmd); err != nil {
		return err
	}
	return nil
}

// cdTarget extracts the destination of a leading `cd <dir>` command.
// Returns ok=false when seg is not a `cd` invocation or has no explicit
// target (bare `cd` / `cd -` go home / previous dir; we leave those to
// the path checks on subsequent tool calls rather than guessing).
func cdTarget(seg string) (string, bool) {
	seg = strings.TrimSpace(seg)
	if seg != "cd" && !strings.HasPrefix(seg, "cd ") {
		return "", false
	}
	arg := strings.TrimSpace(strings.TrimPrefix(seg, "cd"))
	if arg == "" || arg == "-" {
		return "", false
	}
	// Drop surrounding quotes if the model wrapped the path.
	if len(arg) >= 2 {
		if (arg[0] == '"' && arg[len(arg)-1] == '"') || (arg[0] == '\'' && arg[len(arg)-1] == '\'') {
			arg = arg[1 : len(arg)-1]
		}
	}
	return arg, true
}

func (s *Sandbox) checkCommandPaths(cmd string) error {
	for _, tok := range shellWords(cmd) {
		for _, p := range candidatePaths(tok) {
			if err := s.checkCommandPath(p); err != nil {
				return err
			}
		}
	}
	return nil
}

func shellWords(cmd string) []string {
	var words []string
	var b strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if b.Len() == 0 {
			return
		}
		words = append(words, b.String())
		b.Reset()
	}
	for _, r := range cmd {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			b.WriteRune(r)
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			b.WriteRune(r)
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case ' ', '\t', '\n', '\r', ';', '&', '|', '(', ')', '<', '>':
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return words
}

func candidatePaths(tok string) []string {
	if tok == "" || strings.Contains(tok, "://") {
		return nil
	}
	if strings.HasPrefix(tok, "-") {
		if _, value, ok := strings.Cut(tok, "="); ok {
			return candidatePaths(value)
		}
		return nil
	}
	if name, value, ok := strings.Cut(tok, "="); ok && name != "" && value != "" {
		return candidatePaths(value)
	}
	if looksLikePath(tok) {
		return []string{tok}
	}
	return nil
}

func looksLikePath(s string) bool {
	return filepath.IsAbs(s) ||
		strings.HasPrefix(s, "/") ||
		strings.HasPrefix(s, "~") ||
		strings.HasPrefix(s, "$HOME") ||
		strings.HasPrefix(s, "${HOME}") ||
		strings.HasPrefix(s, "..") ||
		strings.Contains(s, "/") ||
		strings.Contains(s, `\\`)
}

func (s *Sandbox) checkCommandPath(path string) error {
	// The platform null device is safe for shell input/output redirection even
	// though it lives outside the sandbox root. Keep this allowlist exact so
	// other device paths and lookalike descendants still fail closed.
	if path == os.DevNull {
		return nil
	}
	rootAbs, err := canonical(s.Root)
	if err != nil {
		return fmt.Errorf("sandbox root: %w", err)
	}
	expanded := expandHome(path)
	if strings.HasPrefix(expanded, "/") && !filepath.IsAbs(expanded) {
		return denied("jailed: path %q is outside sandbox root %q (use /unjail to disable)", path, s.Root)
	}
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(s.Root, filepath.FromSlash(expanded))
	}
	target, err := canonicalOrParent(expanded)
	if err != nil {
		return fmt.Errorf("sandbox path: %w", err)
	}
	if !isUnder(rootAbs, target) {
		return denied("jailed: path %q is outside sandbox root %q (use /unjail to disable)", path, s.Root)
	}
	return nil
}

// checkCDTarget resolves a `cd` destination (relative to the sandbox
// root, with ~ and $HOME expansion) and rejects it only when it lands
// outside the root.
func (s *Sandbox) checkCDTarget(dir string) error {
	rootAbs, err := canonical(s.Root)
	if err != nil {
		return fmt.Errorf("sandbox root: %w", err)
	}
	expanded := expandHome(dir)
	// A leading forward slash is an absolute POSIX path. The shell uses
	// POSIX-style paths regardless of host OS, but on Windows
	// filepath.IsAbs("/etc") is false and filepath.Join would fold it
	// back inside root, so a `cd /etc` escape would slip through. Treat
	// it as an unconditional escape attempt: outside any project root.
	if strings.HasPrefix(expanded, "/") && !filepath.IsAbs(expanded) {
		return denied("jailed: cd outside sandbox root is not allowed (use /unjail to disable)")
	}
	if !filepath.IsAbs(expanded) {
		// Relative targets (including `..`) resolve against the sandbox
		// root, which is the bash tool's working directory when jailed.
		expanded = filepath.Join(s.Root, expanded)
	}
	target, err := canonicalOrParent(expanded)
	if err != nil {
		return fmt.Errorf("sandbox path: %w", err)
	}
	if !isUnder(rootAbs, target) {
		return denied("jailed: cd outside sandbox root is not allowed (use /unjail to disable)")
	}
	return nil
}

// expandHome replaces a leading ~, ~/, or $HOME with the user's home
// directory so cd-target resolution matches what the shell would do.
func expandHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	switch {
	case p == "~" || p == "$HOME" || p == "${HOME}":
		return home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:])
	case strings.HasPrefix(p, "$HOME/"):
		return filepath.Join(home, p[len("$HOME/"):])
	case strings.HasPrefix(p, "${HOME}/"):
		return filepath.Join(home, p[len("${HOME}/"):])
	default:
		return p
	}
}

// canonical returns an absolute, symlink-resolved path. Errors on missing files.
func canonical(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// canonicalOrParent returns the canonical path for p; if p doesn't exist,
// it walks up until it finds an existing directory, then appends the
// remaining path components. This catches symlink-escapes in non-existent
// subtrees (e.g. "new-file" inside a symlinked dir).
func canonicalOrParent(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	// If the full path exists, resolve it.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	// Otherwise, find the longest existing prefix.
	remaining := ""
	current := abs
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, remaining), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return abs, nil
		}
		remaining = filepath.Join(filepath.Base(current), remaining)
		current = parent
	}
}

// isUnder reports whether target is equal to root or a descendant of it.
func isUnder(root, target string) bool {
	rootSep := root
	if !strings.HasSuffix(rootSep, string(filepath.Separator)) {
		rootSep += string(filepath.Separator)
	}
	return target == root || strings.HasPrefix(target, rootSep)
}
