package skills

import (
	"fmt"
	"sort"
	"strings"
)

// Pins records independent global and project selections by discovered name.
type Pins struct {
	Global  []string `json:"global,omitempty"`
	Project []string `json:"project,omitempty"`
}

// Resolve returns each available pinned skill once, ordered by canonical
// skill name (then path). A pin may be a skill's name or one of its aliases;
// two pins that reach the same skill (for example a canonical name and its
// slash alias, or the same name in both scopes) select it once. Names that
// match nothing produce one warning each, in name order.
func (p Pins) Resolve(available []*Skill) ([]*Skill, []string) {
	names := make(map[string]bool)
	for _, name := range append(append([]string(nil), p.Global...), p.Project...) {
		if name != "" {
			names[name] = true
		}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	var selected []*Skill
	chosen := make(map[*Skill]bool)
	var warnings []string
	for _, name := range sorted {
		s := FindByName(available, name)
		if s == nil {
			warnings = append(warnings, fmt.Sprintf("pinned skill %q is unavailable", name))
			continue
		}
		if !chosen[s] {
			chosen[s] = true
			selected = append(selected, s)
		}
	}
	sort.SliceStable(selected, func(i, j int) bool {
		if selected[i].Name != selected[j].Name {
			return selected[i].Name < selected[j].Name
		}
		return selected[i].Path < selected[j].Path
	})
	return selected, warnings
}

// Scopes reports which scopes pin s, resolving aliases the same way Resolve
// does: a pin counts when its name resolves to s among available.
func (p Pins) Scopes(available []*Skill, s *Skill) (project, global bool) {
	hit := func(names []string) bool {
		for _, name := range names {
			if name != "" && FindByName(available, name) == s {
				return true
			}
		}
		return false
	}
	return hit(p.Project), hit(p.Global)
}

// PreloadPrompt includes pinned instructions with the first real request.
// It creates no synthetic tool calls and does not start a model turn.
func PreloadPrompt(selected []*Skill, request string) string {
	if len(selected) == 0 {
		return request
	}
	var b strings.Builder
	b.WriteString("Pinned skills for this conversation. Apply these instructions where relevant to the user's requests.\n\n")
	for _, s := range selected {
		b.WriteString(InvocationPrompt(s, ""))
		b.WriteString("\n\n")
	}
	b.WriteString("---\n\nUser request:\n")
	b.WriteString(request)
	return b.String()
}
