// Package subagents discovers named agent profiles that can be selected by
// zut's resident subagent runtime. Profiles use the common markdown/frontmatter layout:
// a YAML-like metadata block followed by the agent's system prompt.
//
// Discovery prefers explicitly configured directories, then the shared
// ~/.agents/agents directory. Profiles are metadata plus instructions; the
// main agent sees only the compact manifest, while a selected child loads the
// full profile itself.
package subagents

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const maxProfileBytes = 1 << 20

// Profile is one discovered named subagent definition.
type Profile struct {
	Name         string
	Description  string
	SystemPrompt string
	Tools        []string
	// ToolsDeclared distinguishes an omitted tools key (inherit the
	// child-safe catalogue) from an explicit empty list (grant no tools).
	ToolsDeclared bool
	Model         string
	Provider      string
	Thinking      string

	// Nil means the profile did not specify whether fast mode is enabled.
	FastMode *bool

	// SystemPromptMode is "append" (the default) or "replace".
	SystemPromptMode string

	// Nil means the profile did not specify the inheritance behavior.
	InheritProjectContext *bool
	InheritSkills         *bool

	Path   string
	Source string
}

// Discover returns profiles in precedence order, sorted by name. A profile
// with the same name in a higher-priority directory shadows lower-priority
// copies. Missing directories are normal and do not produce errors.
func Discover(cwd, userHome string) ([]*Profile, []error) {
	seen := make(map[string]*Profile)
	var errs []error

	for _, loc := range searchDirs(cwd, userHome) {
		entries, err := os.ReadDir(loc.dir)
		if err != nil {
			if os.IsNotExist(err) {
				if info, statErr := os.Stat(loc.dir); statErr == nil && !info.IsDir() {
					errs = append(errs, fmt.Errorf("read subagent directory %s: not a directory", loc.dir))
				}
			} else {
				errs = append(errs, fmt.Errorf("read subagent directory %s: %w", loc.dir, err))
			}
			continue
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
				continue
			}
			path := filepath.Join(loc.dir, entry.Name())
			profile, err := load(path, loc.label)
			if err != nil {
				if !os.IsNotExist(err) {
					errs = append(errs, fmt.Errorf("read subagent profile %s: %w", path, err))
				}
				continue
			}
			if !validName(profile.Name) || profile.SystemPrompt == "" {
				continue
			}
			if _, exists := seen[profile.Name]; exists {
				continue
			}
			seen[profile.Name] = profile
		}
	}

	out := make([]*Profile, 0, len(seen))
	for _, profile := range seen {
		out = append(out, profile)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

// Find returns a profile by its exact name, or nil when it is unavailable.
func Find(profiles []*Profile, name string) *Profile {
	name = strings.TrimSpace(name)
	for _, profile := range profiles {
		if profile != nil && profile.Name == name {
			return profile
		}
	}
	return nil
}

// ModelSelection splits a provider-qualified model value such as
// "openai-codex/gpt-5.6-luna". Explicit provider metadata must be paired
// with an unqualified model; model IDs may contain slashes when no provider
// metadata is supplied.
func (p *Profile) ModelSelection() (provider, model string) {
	if p == nil {
		return "", ""
	}
	provider = strings.TrimSpace(p.Provider)
	model = strings.TrimSpace(p.Model)
	if provider == "" {
		if before, after, ok := strings.Cut(model, "/"); ok {
			provider, model = strings.TrimSpace(before), strings.TrimSpace(after)
		}
	}
	return provider, model
}

// ResolveProfileTools applies a profile's exact frontmatter declaration to a
// host-provided child-safe catalogue. An omitted declaration inherits that
// catalogue; an explicit empty list grants no tools. Explicit names outside
// the child catalogue or denied by host policy are omitted so portable profile
// files can name host-only extensions without granting them to a child.
func ResolveProfileTools(profile *Profile, childSafe []string, permitted func(string) bool) ([]string, error) {
	if profile == nil || !profile.ToolsDeclared {
		return append([]string(nil), childSafe...), nil
	}
	catalogue := make(map[string]struct{}, len(childSafe))
	for _, name := range childSafe {
		catalogue[name] = struct{}{}
	}
	resolved := make([]string, 0, len(profile.Tools))
	for _, name := range profile.Tools {
		name = strings.TrimSpace(name)
		if _, ok := catalogue[name]; !ok {
			continue
		}
		if permitted != nil && !permitted(name) {
			continue
		}
		resolved = append(resolved, name)
	}
	return resolved, nil
}

// SystemPromptAddendum renders the compact [subagents_list] manifest for the
// parent model. Full profile bodies are intentionally omitted: the selected
// child loads its own instructions, and keeping them out of every parent turn
// avoids unnecessary context and accidental instruction mixing.
func SystemPromptAddendum(profiles []*Profile) string {
	if len(profiles) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[subagents_list]\n")
	sb.WriteString("Named subagents available to subagent_spawn. Choose the profile whose description best matches the independent task and pass its name as the tool's agent field. The selected profile's instructions, model, thinking level, tool limits, fast-mode preference, and budget override are applied to the child.\n")
	for _, profile := range profiles {
		if profile == nil {
			continue
		}
		name := manifestValue(profile.Name)
		description := manifestValue(profile.Description)
		if description == "" {
			description = "(no description)"
		}
		metadata := make([]string, 0, 7)
		if model := manifestValue(profile.Model); model != "" {
			metadata = append(metadata, "model="+model)
		}
		if provider := manifestValue(profile.Provider); provider != "" {
			metadata = append(metadata, "provider="+provider)
		}
		if thinking := manifestValue(profile.Thinking); thinking != "" {
			metadata = append(metadata, "thinking="+thinking)
		}
		if len(profile.Tools) > 0 {
			metadata = append(metadata, "tools="+manifestValue(strings.Join(profile.Tools, ",")))
		}
		if profile.FastMode != nil {
			metadata = append(metadata, "fastMode="+strconv.FormatBool(*profile.FastMode))
		}
		if len(metadata) > 0 {
			fmt.Fprintf(&sb, "- %s [%s]: %s\n", name, strings.Join(metadata, " "), description)
		} else {
			fmt.Fprintf(&sb, "- %s: %s\n", name, description)
		}
	}
	sb.WriteString("[/subagents_list]")
	return sb.String()
}

type location struct {
	dir   string
	label string
}

func searchDirs(cwd, userHome string) []location {
	var out []location
	add := func(dir, label string) {
		if strings.TrimSpace(dir) != "" {
			out = append(out, location{dir: dir, label: label})
		}
	}
	if extra := os.Getenv("ZUT_AGENT_PROFILES"); extra != "" {
		for _, dir := range filepath.SplitList(extra) {
			dir = strings.TrimSpace(dir)
			if dir != "" && !filepath.IsAbs(dir) && strings.TrimSpace(cwd) != "" {
				dir = filepath.Join(cwd, dir)
			}
			add(dir, "configured")
		}
	}
	if userHome != "" {
		add(filepath.Join(userHome, ".agents", "agents"), "global (agents)")
	}
	return out
}

func load(path, source string) (*Profile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if info.Size() > maxProfileBytes {
		return nil, fmt.Errorf("profile is larger than %d bytes", maxProfileBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	front, body, err := splitFrontmatter(string(raw))
	if err != nil {
		return nil, err
	}
	values, lists := parseFrontmatter(front)
	name := unquote(values["name"])
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	thinking, err := parseThinkingValue(values["thinking"], "thinking")
	if err != nil {
		return nil, err
	}
	reasoning, err := parseThinkingValue(values["reasoning"], "reasoning")
	if err != nil {
		return nil, err
	}
	if thinking == "" {
		thinking = reasoning
	}
	model := strings.TrimSpace(unquote(values["model"]))
	provider := strings.TrimSpace(unquote(values["provider"]))
	if provider != "" && strings.Contains(model, "/") {
		return nil, fmt.Errorf("model must not include a provider when provider is set")
	}
	_, toolsDeclared := lists["tools"]
	profile := &Profile{
		Name:          strings.TrimSpace(name),
		Description:   strings.TrimSpace(unquote(values["description"])),
		SystemPrompt:  strings.TrimSpace(body),
		Tools:         lists["tools"],
		ToolsDeclared: toolsDeclared,
		Model:         model,
		Provider:      provider,
		Thinking:      thinking,
		Path:          path,
		Source:        source,
	}
	profile.SystemPromptMode = "append"
	if mode, ok := values["systempromptmode"]; ok {
		switch strings.ToLower(strings.TrimSpace(unquote(mode))) {
		case "", "append":
		case "replace":
			profile.SystemPromptMode = "replace"
		default:
			return nil, fmt.Errorf("systemPromptMode must be append or replace")
		}
	}
	if raw, ok := values["inheritprojectcontext"]; ok {
		value, parsed := parseOptionalBool(raw)
		if !parsed {
			return nil, fmt.Errorf("inheritProjectContext must be true or false")
		}
		profile.InheritProjectContext = &value
	}
	if raw, ok := values["inheritskills"]; ok {
		value, parsed := parseOptionalBool(raw)
		if !parsed {
			return nil, fmt.Errorf("inheritSkills must be true or false")
		}
		profile.InheritSkills = &value
	}
	if raw, ok := values["fastmode"]; ok {
		value, parsed := parseOptionalBool(raw)
		if !parsed {
			return nil, fmt.Errorf("fastMode must be true or false")
		}
		profile.FastMode = &value
	}
	return profile, nil
}

func splitFrontmatter(raw string) (front, body string, err error) {
	rest := strings.TrimLeft(raw, " \t\r\n")
	firstEnd := strings.IndexByte(rest, '\n')
	if firstEnd < 0 || strings.TrimSpace(rest[:firstEnd]) != "---" {
		return "", raw, nil
	}
	rest = rest[firstEnd+1:]
	for offset := 0; offset <= len(rest); {
		rel := strings.Index(rest[offset:], "\n---")
		if rel < 0 {
			// A document that starts a frontmatter block but never closes
			// it is malformed, not a body-only profile.
			return "", "", fmt.Errorf("frontmatter: missing closing delimiter")
		}
		end := offset + rel
		after := end + len("\n---")
		if after == len(rest) || rest[after] == '\n' || rest[after] == '\r' {
			return rest[:end], strings.TrimLeft(rest[after:], " \t\r\n"), nil
		}
		offset = after
	}
	return "", raw, nil
}

func parseFrontmatter(front string) (map[string]string, map[string][]string) {
	values := make(map[string]string)
	lists := make(map[string][]string)
	lines := strings.Split(front, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		colon := strings.IndexByte(trimmed, ':')
		if colon < 0 {
			continue
		}
		key := normalizeKey(trimmed[:colon])
		value := strings.TrimSpace(trimmed[colon+1:])
		if value == "" {
			var items []string
			for j := i + 1; j < len(lines); j++ {
				next := strings.TrimSpace(lines[j])
				if next == "" {
					continue
				}
				if !strings.HasPrefix(next, "-") || (len(lines[j]) > 0 && !strings.HasPrefix(lines[j], " ") && !strings.HasPrefix(lines[j], "\t")) {
					break
				}
				items = append(items, unquote(strings.TrimSpace(strings.TrimPrefix(next, "-"))))
				i = j
			}
			if key == "tools" || len(items) > 0 {
				lists[key] = items
			}
			continue
		}
		if key == "tools" {
			lists[key] = parseList(value)
			continue
		}
		values[key] = unquote(value)
	}
	return values, lists
}

func normalizeKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "-", "")
	key = strings.ReplaceAll(key, "_", "")
	return key
}

func parseList(value string) []string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if item := strings.TrimSpace(unquote(part)); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func parseThinkingValue(value, field string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(unquote(value))) {
	case "":
		return "", nil
	case "off":
		return "off", nil
	case "minimal", "minimum":
		return "minimum", nil
	case "low", "medium", "high", "xhigh", "max":
		return strings.ToLower(strings.TrimSpace(unquote(value))), nil
	case "maximum":
		return "xhigh", nil
	default:
		return "", fmt.Errorf("%s must be off|minimum|low|medium|high|xhigh|max", field)
	}
}

func parseOptionalBool(value string) (bool, bool) {
	parsed, err := strconv.ParseBool(strings.TrimSpace(unquote(value)))
	return parsed, err == nil
}

func validName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r == '\x00' || r < 0x20 {
			return false
		}
	}
	return true
}

func manifestValue(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func unquote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		return value[1 : len(value)-1]
	}
	return value
}
