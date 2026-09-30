// Package skills implements zut's reusable-instruction system.
//
// A skill is a per-folder SKILL.md file with a YAML frontmatter
// header. Skills live in well-known directories under the project or
// the user home; zut discovers them at startup, lists their names +
// one-line descriptions in the system prompt, and exposes a built-in
// "skill" tool the model uses to pull the full body on demand.
//
// The on-demand-load model keeps token usage cheap: only the
// short manifest goes into every request; the body is fetched as a
// tool result the one or two turns the model actually needs it.
//
// Discovery layout (priority order — first match wins per name):
//
//	./.zut/skills/<name>/SKILL.md            — project (native)
//	$ZUT_HOME/skills/<name>/SKILL.md         — global (native)
//	./.claude/skills/<name>/SKILL.md         — project (claude-compat)
//	~/.claude/skills/<name>/SKILL.md         — global (claude-compat)
//	./.agents/skills/<name>/SKILL.md         — project (agent-compat)
//	~/.agents/skills/<name>/SKILL.md         — global (agent-compat)
//
// The compat paths are deliberate: a SKILL.md written for any of
// the related ecosystems works in zut unchanged. User skill roots are
// walked recursively, and nested files can also be addressed by their
// slash-separated path relative to the root (for example,
// systems-backend/subskills/golang-patterns). Symlinked directories are
// followed; see scanUserSkills.
package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
)

// Skill is one discovered SKILL.md file.
type Skill struct {
	// Name is the skill identifier — what the model uses when it
	// invokes the `skill` tool. Taken from the frontmatter `name`
	// field; falls back to the directory basename.
	Name string

	// Aliases are alternate identifiers accepted by FindByName. Nested
	// user skills include their slash-separated path relative to the
	// search root, so routers can refer to them by file-system path.
	Aliases []string

	// Description is the one-line summary shown to the model in the
	// system-prompt manifest.
	Description string

	// Body is the markdown after the frontmatter. Returned as the
	// tool result when the model loads this skill.
	Body string

	// Path is the absolute path to the SKILL.md file.
	Path string

	// Source is a human-friendly label describing where the skill
	// came from ("project", "global", "project (claude)", etc.).
	// Shown in the /skills picker.
	Source string

	// Builtin marks skills that ship inside the zut binary. They are
	// fully active for the model (system-prompt manifest + skill
	// tool) but hidden from user-facing surfaces like the /skills
	// picker so users only see skills they actually installed or
	// shipped in their project.
	Builtin bool

	// DisableModelInvocation hides the skill from the system-prompt
	// manifest. The user can still load it explicitly with
	// /skill:<name>.
	DisableModelInvocation bool

	// AllowedTools and Permissions are parsed for forward-
	// compatibility but NOT enforced in this version. They appear
	// in the skill body so the model can self-regulate.
	AllowedTools []string
	Permissions  map[string][]string
}

// BundledSkillDir identifies a skill directory shipped by an extension.
// The directory contains one subdirectory per skill, each with SKILL.md.
// Root is optional; when set, every loaded SKILL.md must resolve beneath it.
// Extension managers set Root to keep symlinked skill files inside the
// extension directory.
type BundledSkillDir struct {
	Dir    string
	Source string
	Root   string
}

// VisibleSkills returns the subset of skills users should see in
// pickers, /skills, and other interactive surfaces. Built-ins are
// hidden because they're implementation detail; the model still
// loads them through the system-prompt manifest + the skill tool.
func VisibleSkills(in []*Skill) []*Skill {
	out := make([]*Skill, 0, len(in))
	for _, s := range in {
		if s == nil || s.Builtin {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Discover returns the merged skill set. When includeUser is true,
// user-installed SKILL.md files are loaded before built-ins. Callers
// normally pass true; --no-skill skips discovery entirely before this
// function is called.
//
// First-match-wins per name; the order matches the priority list
// in the package doc (project-local before global before claude-
// compat before agents-compat, all before built-ins). That means a
// user-installed skill with the same name as a built-in shadows
// the built-in once includeUser is true.
//
// Errors per skill are returned alongside the partial result so a
// single broken file doesn't suppress the rest.
func Discover(zutHome, cwd, userHome string, includeUser bool) ([]*Skill, []error) {
	return DiscoverWithBundled(zutHome, cwd, userHome, includeUser, nil)
}

// DiscoverWithBundled merges normal user skills with extension-bundled skills
// and built-ins. Precedence is user/project locations, then bundled skills in
// declaration order, then built-ins. The first matching name wins.
func DiscoverWithBundled(zutHome, cwd, userHome string, includeUser bool, bundled []BundledSkillDir) ([]*Skill, []error) {
	var errs []error
	seen := map[string]*Skill{}
	if includeUser {
		errs = append(errs, scanUserSkills(zutHome, cwd, userHome, seen)...)
	}
	for _, dir := range bundled {
		errs = append(errs, scanBundledSkills(dir, seen)...)
	}
	// Built-ins fill in any name the user or an extension didn't provide.
	for _, s := range loadBuiltins() {
		registerSkill(seen, s)
	}
	out := uniqueSkills(seen)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

// LoadBundled loads only the skills declared by extension manifests. The
// first declaration with a given name wins; built-in and user locations are
// intentionally not considered here.
func LoadBundled(bundled []BundledSkillDir) ([]*Skill, []error) {
	seen := map[string]*Skill{}
	var errs []error
	for _, dir := range bundled {
		errs = append(errs, scanBundledSkills(dir, seen)...)
	}
	out := uniqueSkills(seen)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

func pathWithinRoot(root, path string) (bool, error) {
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false, err
	}
	rootAbs, err := filepath.Abs(rootResolved)
	if err != nil {
		return false, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, err
	}
	resolvedAbs, err := filepath.Abs(resolved)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(rootAbs, resolvedAbs)
	if err != nil {
		return false, err
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

// scanBundledSkills walks one caller-resolved bundled-skill directory and
// populates `seen` with first-match-wins per name.
func scanBundledSkills(dir BundledSkillDir, seen map[string]*Skill) []error {
	if strings.TrimSpace(dir.Dir) == "" {
		return nil
	}
	label := strings.TrimSpace(dir.Source)
	if label == "" {
		label = "extension (unnamed)"
	}
	var errs []error
	loadOne := func(path, fallbackName string) {
		if dir.Root != "" {
			safe, err := pathWithinRoot(dir.Root, path)
			if err != nil {
				if !os.IsNotExist(err) {
					errs = append(errs, fmt.Errorf("%s: validate path: %w", path, err))
				}
				return
			}
			if !safe {
				errs = append(errs, fmt.Errorf("%s: resolved path escapes bundled extension directory", path))
				return
			}
		}
		s, err := load(path, label)
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("%s: %w", path, err))
			}
			return
		}
		if s.Name == "" {
			s.Name = fallbackName
		}
		registerSkill(seen, s)
	}
	skillPath := filepath.Join(dir.Dir, "SKILL.md")
	if info, err := os.Stat(skillPath); err == nil {
		if !info.IsDir() {
			loadOne(skillPath, filepath.Base(dir.Dir))
			return errs
		}
	} else if !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("%s: %w", skillPath, err))
	}
	entries, err := os.ReadDir(dir.Dir)
	if err != nil {
		if !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("%s: %w", dir.Dir, err))
		}
		return errs
	}
	// Real directories load before symlinked ones so a link cannot claim a
	// fallback name ahead of the real directory it points at. Each entry
	// stays one level deep, and loadOne still confines every resolved
	// SKILL.md to dir.Root. A directory reachable both directly and through a
	// link is loaded once, so an unnamed skill is not duplicated under its
	// alias name.
	visited := map[string]bool{}
	visit := func(path string) bool {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			if !skipLinkError(err) {
				errs = append(errs, fmt.Errorf("%s: %w", path, err))
			}
			return false
		}
		if visited[resolved] {
			return false
		}
		visited[resolved] = true
		return true
	}
	var linked []string
	for _, entry := range entries {
		if entry.IsDir() {
			path := filepath.Join(dir.Dir, entry.Name())
			if visit(path) {
				loadOne(filepath.Join(path, "SKILL.md"), entry.Name())
			}
			continue
		}
		if entry.Type()&fs.ModeSymlink == 0 {
			continue
		}
		path := filepath.Join(dir.Dir, entry.Name())
		info, statErr := os.Stat(path)
		if statErr != nil {
			// Dangling and cyclic links are skipped; anything else
			// (permissions, I/O) is reported.
			if !skipLinkError(statErr) {
				errs = append(errs, fmt.Errorf("%s: %w", path, statErr))
			}
			continue
		}
		if info.IsDir() {
			linked = append(linked, entry.Name())
		}
	}
	for _, name := range linked {
		path := filepath.Join(dir.Dir, name)
		if visit(path) {
			loadOne(filepath.Join(path, "SKILL.md"), name)
		}
	}
	return errs
}

// scanUserSkills walks the user-skill search dirs recursively and
// populates `seen` with first-match-wins per name or alias. Split out
// so Discover's includeUser=false path doesn't have to skip over a
// giant block.
//
// Symlinked directories are followed (filepath.WalkDir would not). Real
// directories are scanned before links so a link cannot claim a name or
// alias ahead of the real tree, every resolved directory is scanned once so
// link cycles terminate, and dangling or cyclic links are skipped. Skills
// reached through a link keep the walked (link) path.
func scanUserSkills(zutHome, cwd, userHome string, seen map[string]*Skill) []error {
	var errs []error
	for _, loc := range searchDirs(zutHome, cwd, userHome) {
		errs = append(errs, scanUserSkillDir(loc, seen)...)
	}
	return errs
}

// Traversal bounds for one user skill location. Real skill trees are shallow
// and small; the caps stop a pathological or mis-linked tree (for example a
// link into a huge shared checkout) from stalling startup. Too-deep
// subtrees are skipped and the directory-count cap ends the scan of that
// location; either way one nonfatal diagnostic is reported and every skill
// already found is kept.
const (
	maxSkillScanDepth = 64
	maxSkillScanDirs  = 10000
)

type linkedDir struct {
	path  string
	depth int
}

func scanUserSkillDir(loc location, seen map[string]*Skill) []error {
	var errs []error
	visited := map[string]bool{}
	var linkedDirs []linkedDir
	dirCount := 0
	truncated := false // directory cap reached: stop the whole location
	reported := false
	report := func(why string) {
		if !reported {
			reported = true
			errs = append(errs, fmt.Errorf("%s: skill scan stopped early: %s; some directories were skipped", loc.dir, why))
		}
	}
	// A link whose target is the location itself or one of its ancestors
	// would pull the surrounding filesystem into the scan. Compare against
	// the resolved location root.
	rootResolved, rootErr := filepath.EvalSymlinks(loc.dir)
	loadSkill := func(path string) {
		s, err := load(path, loc.label)
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("%s: %w", path, err))
			}
			return
		}
		if s.Name == "" {
			s.Name = filepath.Base(filepath.Dir(path))
		}
		addRelativeAlias(s, loc.dir, path)
		registerSkill(seen, s)
	}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if truncated {
			return
		}
		if depth > maxSkillScanDepth {
			report(fmt.Sprintf("nesting deeper than %d levels", maxSkillScanDepth))
			return
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			if !skipLinkError(err) {
				errs = append(errs, fmt.Errorf("%s: %w", dir, err))
			}
			return
		}
		if visited[resolved] {
			return
		}
		if dirCount >= maxSkillScanDirs {
			truncated = true
			report(fmt.Sprintf("more than %d directories", maxSkillScanDirs))
			return
		}
		visited[resolved] = true
		dirCount++
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("%s: %w", dir, err))
			}
			return
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			isDir := entry.IsDir()
			if entry.Type()&fs.ModeSymlink != 0 {
				info, statErr := os.Stat(path)
				if statErr != nil {
					// Dangling and cyclic links are not skills; SKILL.md
					// links fall through to load, which ignores ENOENT.
					if !skipLinkError(statErr) {
						errs = append(errs, fmt.Errorf("%s: %w", path, statErr))
					}
					continue
				}
				if info.IsDir() {
					if target, err := filepath.EvalSymlinks(path); err == nil && rootErr == nil && containsPath(target, rootResolved) {
						// Link to the root or an ancestor of it: a cycle.
						continue
					}
					linkedDirs = append(linkedDirs, linkedDir{path: path, depth: depth + 1})
					continue
				}
			}
			if isDir {
				walk(path, depth+1)
				continue
			}
			if entry.Name() != "SKILL.md" {
				continue
			}
			if dir == loc.dir {
				// Keep the existing convention that a search root is a
				// collection of skill directories, not a skill itself.
				continue
			}
			loadSkill(path)
		}
	}
	walk(loc.dir, 0)
	for i := 0; i < len(linkedDirs); i++ {
		walk(linkedDirs[i].path, linkedDirs[i].depth)
	}
	return errs
}

// containsPath reports whether child equals parent or lies beneath it. Both
// paths must already be resolved and absolute.
func containsPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// skipLinkError reports whether err means a link points at nothing or loops.
func skipLinkError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || symlinkLoopError(err)
}

func symlinkLoopError(err error) bool {
	// Windows reports cyclic links as ERROR_CANT_RESOLVE_FILENAME (1921),
	// rather than ELOOP. syscall does not export a name for this error.
	return errors.Is(err, syscall.ELOOP) ||
		(runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1921)))
}

// addRelativeAlias makes a nested skill addressable by the directory path
// used by common skill routers, for example
// systems-backend/subskills/golang-patterns.
func addRelativeAlias(s *Skill, root, path string) {
	if s == nil {
		return
	}
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil || rel == "." || rel == "" {
		return
	}
	alias := filepath.ToSlash(rel)
	if alias == s.Name {
		return
	}
	for _, existing := range s.Aliases {
		if existing == alias {
			return
		}
	}
	s.Aliases = append(s.Aliases, alias)
}

// registerSkill records a skill under its canonical name and aliases. A
// collision on any identifier means a higher-priority skill already owns
// that identifier, so the new skill is skipped as one unit.
func registerSkill(seen map[string]*Skill, s *Skill) bool {
	if s == nil || s.Name == "" {
		return false
	}
	keys := append([]string{s.Name}, s.Aliases...)
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			return false
		}
	}
	for _, key := range keys {
		if key != "" {
			seen[key] = s
		}
	}
	return true
}

func uniqueSkills(seen map[string]*Skill) []*Skill {
	out := make([]*Skill, 0, len(seen))
	added := make(map[*Skill]struct{}, len(seen))
	for _, s := range seen {
		if s == nil {
			continue
		}
		if _, exists := added[s]; exists {
			continue
		}
		added[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// SystemPromptAddendum returns the text to append to the system
// prompt when at least one skill is loaded. Empty string if none.
//
// The format is deliberately compact: name, one-line description,
// and a source pointer telling the model where the full body
// lives. Built-in skills show "builtin" since their markdown is
// embedded in the zut binary and not on the filesystem; user
// skills show their SKILL.md path (shortened with ~ for HOME).
//
// Loading still goes through the `skill` tool with just the name.
// The pointer is there so the model can (a) mention the source
// honestly in explanations and (b) distinguish between built-ins
// and user-authored instruction sets when reasoning about trust.
func SystemPromptAddendum(skills []*Skill) string {
	home, _ := os.UserHomeDir()
	var sb strings.Builder
	for _, s := range skills {
		if s == nil || s.DisableModelInvocation {
			continue
		}
		if sb.Len() == 0 {
			sb.WriteString("Available skills (call the `skill` tool with a name from this list to load its full instructions):\n")
		}
		desc := strings.TrimSpace(s.Description)
		if desc == "" {
			desc = "(no description)"
		}
		pointer := skillSourcePointer(s, home)
		fmt.Fprintf(&sb, "- %s [%s]: %s\n", s.Name, pointer, desc)
	}
	return sb.String()
}

// skillSourcePointer returns a short tag describing where a skill
// originates. Built-ins are tagged "builtin" because their markdown
// is embedded in the zut binary and not reachable through the
// filesystem. User skills are tagged with their SKILL.md path,
// collapsed to use ~ for the user home when possible.
func skillSourcePointer(s *Skill, home string) string {
	if s == nil {
		return "unknown"
	}
	if s.Builtin {
		return "builtin"
	}
	p := s.Path
	if p == "" {
		return "unknown"
	}
	if home != "" && strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}

// FindByName returns the skill with the given name or alias, or nil.
func FindByName(skills []*Skill, name string) *Skill {
	for _, s := range skills {
		if s != nil && s.Name == name {
			return s
		}
	}
	for _, s := range skills {
		if s == nil {
			continue
		}
		for _, alias := range s.Aliases {
			if alias == name {
				return s
			}
		}
	}
	return nil
}

// ---- internals ----

type location struct {
	dir   string
	label string
}

func searchDirs(zutHome, cwd, userHome string) []location {
	var out []location
	add := func(dir, label string) {
		if dir == "" {
			return
		}
		out = append(out, location{dir: dir, label: label})
	}
	if extra := os.Getenv("ZUT_AGENT_SKILLS"); extra != "" {
		for _, dir := range filepath.SplitList(extra) {
			add(dir, "agent")
		}
	}
	if cwd != "" {
		add(filepath.Join(cwd, ".zut", "skills"), "project")
	}
	if zutHome != "" {
		add(filepath.Join(zutHome, "skills"), "global")
	}
	if cwd != "" {
		add(filepath.Join(cwd, ".claude", "skills"), "project (claude)")
	}
	if userHome != "" {
		add(filepath.Join(userHome, ".claude", "skills"), "global (claude)")
	}
	if cwd != "" {
		add(filepath.Join(cwd, ".agents", "skills"), "project (agents)")
	}
	if userHome != "" {
		add(filepath.Join(userHome, ".agents", "skills"), "global (agents)")
	}
	return out
}

func load(path, source string) (*Skill, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	front, body := splitFrontmatter(string(raw))
	s := &Skill{
		Path:   path,
		Source: source,
		Body:   strings.TrimSpace(body),
	}
	parseFrontmatter(front, s)
	return s, nil
}

// splitFrontmatter returns (yamlBlock, restOfDocument) for a string
// whose first non-empty line is "---". If no frontmatter is present,
// returns ("", entireString).
func splitFrontmatter(raw string) (string, string) {
	rest := strings.TrimLeft(raw, " \t\r\n")
	if !strings.HasPrefix(rest, "---") {
		return "", raw
	}
	rest = strings.TrimPrefix(rest, "---")
	// Drop the trailing newline after the opening ---.
	if i := strings.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[i+1:]
	}
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", raw // malformed; treat as no frontmatter
	}
	front := rest[:end]
	body := rest[end+len("\n---"):]
	body = strings.TrimLeft(body, " \t\r\n")
	return front, body
}

// parseFrontmatter handles the small subset of YAML zut recognizes:
//   - simple `key: value` lines
//   - `key: [a, b, c]` flow-style lists
//   - `key:` followed by indented `- item` block lists
//   - nested `key:` followed by indented `subkey: [...]` for permissions
//
// Anything more elaborate is ignored. We deliberately avoid a yaml
// dependency to keep zut's binary lean.
func parseFrontmatter(front string, s *Skill) {
	lines := strings.Split(front, "\n")
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			i++
			continue
		}
		// Top-level key: value or key:
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			colon := strings.IndexByte(trimmed, ':')
			if colon < 0 {
				i++
				continue
			}
			key := strings.TrimSpace(trimmed[:colon])
			value := strings.TrimSpace(trimmed[colon+1:])
			switch key {
			case "name":
				s.Name = unquote(value)
			case "description":
				s.Description = unquote(value)
			case "disable-model-invocation", "disable_model_invocation":
				s.DisableModelInvocation = strings.EqualFold(unquote(value), "true")
			case "allowed-tools", "allowed_tools":
				if value != "" {
					s.AllowedTools = parseInlineList(value)
				} else {
					items, consumed := parseBlockList(lines[i+1:])
					s.AllowedTools = items
					i += consumed
				}
			case "permissions":
				if value != "" {
					// Unusual layout; ignore single-line for now.
					i++
					continue
				}
				perms, consumed := parsePermissionsBlock(lines[i+1:])
				s.Permissions = perms
				i += consumed
			}
		}
		i++
	}
}

// unquote trims surrounding " or ' from a value.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// parseInlineList parses "[a, b, c]" or "[\"a\", \"b\"]".
func parseInlineList(v string) []string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, unquote(strings.TrimSpace(p)))
	}
	return out
}

// parseBlockList consumes indented "- item" lines until a less-
// indented line. Returns the items + how many lines to skip.
func parseBlockList(lines []string) ([]string, int) {
	var out []string
	consumed := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "  - ") || strings.HasPrefix(line, "    - ") {
			out = append(out, unquote(strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(line, " "), "-"))))
			consumed++
			continue
		}
		if strings.TrimSpace(line) == "" {
			consumed++
			continue
		}
		break
	}
	return out, consumed
}

// parsePermissionsBlock parses an indented map of tool->[patterns].
//
//	permissions:
//	  bash: ["git diff*", "git log*"]
//	  read: ["./*.go"]
func parsePermissionsBlock(lines []string) (map[string][]string, int) {
	out := map[string][]string{}
	consumed := 0
	for _, line := range lines {
		if !strings.HasPrefix(line, "  ") {
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			consumed++
			continue
		}
		colon := strings.IndexByte(trimmed, ':')
		if colon < 0 {
			break
		}
		key := strings.TrimSpace(trimmed[:colon])
		val := strings.TrimSpace(trimmed[colon+1:])
		if val == "" {
			break // we don't support nested-block lists for permissions
		}
		out[key] = parseInlineList(val)
		consumed++
	}
	return out, consumed
}
