package tools

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bnema/zut/packages/core"
)

// denied marks a policy refusal (permission scope, allowlist, jail
// confinement, or admission gate) so core can distinguish it from an
// ordinary tool failure. The message text shown to the model is unchanged.
func denied(format string, args ...any) error {
	return &core.ToolDeniedError{Reason: fmt.Sprintf(format, args...)}
}

// PermissionSet is the local-runtime permission contract for a packaged .zut agent.
type PermissionSet struct {
	FS struct {
		Read  []string `json:"read"`
		Write []string `json:"write"`
	} `json:"fs"`
	Bash struct {
		Mode  string   `json:"mode"`
		Allow []string `json:"allow"`
	} `json:"bash"`
	Net struct {
		Allow []string `json:"allow"`
	} `json:"net"`
	Env struct {
		Read []string `json:"read"`
	} `json:"env"`
}

// Expand resolves manifest variables into absolute local paths.
func (p PermissionSet) Expand(workspace, agentData string) PermissionSet {
	expand := func(s string) string {
		s = strings.ReplaceAll(s, "${workspace}", workspace)
		s = strings.ReplaceAll(s, "${agent_data}", agentData)
		if !filepath.IsAbs(s) && s != "" {
			s = filepath.Join(workspace, s)
		}
		return s
	}
	// Clone the slices before expanding them. PermissionSet is copied by value,
	// but slices otherwise still alias the manifest and repeated expansion (for
	// example after /cd) would mutate the original declaration.
	p.FS.Read = append([]string(nil), p.FS.Read...)
	p.FS.Write = append([]string(nil), p.FS.Write...)
	for i, v := range p.FS.Read {
		p.FS.Read[i] = expand(v)
	}
	for i, v := range p.FS.Write {
		p.FS.Write[i] = expand(v)
	}
	return p
}

func (s *Sandbox) SetPermissions(p *PermissionSet) {
	if s == nil {
		return
	}
	s.Permissions = p
}

func (s *Sandbox) CheckReadPath(path string) error {
	if err := s.CheckPath(path); err != nil {
		return err
	}
	if s == nil || s.Permissions == nil {
		return nil
	}
	if len(s.Permissions.FS.Read) == 0 {
		return denied("permission denied: this agent has no filesystem read permission")
	}
	return checkScopedPath("read", path, s.Permissions.FS.Read)
}

func (s *Sandbox) CheckWritePath(path string) error {
	if err := s.CheckPath(path); err != nil {
		return err
	}
	if s == nil || s.Permissions == nil {
		return nil
	}
	if len(s.Permissions.FS.Write) == 0 {
		return denied("permission denied: this agent has no filesystem write permission")
	}
	return checkScopedPath("write", path, s.Permissions.FS.Write)
}

func checkScopedPath(op, path string, scopes []string) error {
	target, err := canonicalOrParent(path)
	if err != nil {
		return fmt.Errorf("permission path: %w", err)
	}
	for _, scope := range scopes {
		root, err := canonicalOrParent(scope)
		if err != nil {
			continue
		}
		if isUnder(root, target) {
			return nil
		}
	}
	return denied("permission denied: %s %q is outside declared scopes", op, path)
}

func (s *Sandbox) CheckBashPermission(cmd string) error {
	if s == nil || s.Permissions == nil {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(s.Permissions.Bash.Mode))
	if mode == "" {
		mode = "none"
	}
	switch mode {
	case "none":
		return denied("permission denied: this agent has no bash permission")
	case "ask":
		// The local runtime obtains explicit consent for this capability at
		// every launch. Unlike durable consent, ask mode is never cached.
		return nil
	case "allowlist":
		if strings.ContainsAny(cmd, "`\n\r<>") || strings.Contains(cmd, "$(") {
			return denied("permission denied: bash allowlist does not permit substitution or redirection")
		}
		names := commandNames(cmd)
		if len(names) == 0 {
			return denied("permission denied: empty command")
		}
		allowed := map[string]bool{}
		for _, name := range s.Permissions.Bash.Allow {
			allowed[filepath.Base(strings.TrimSpace(name))] = true
		}
		for _, name := range names {
			if !allowed[name] {
				return denied("permission denied: bash command %q is not in allowlist", name)
			}
		}
		return nil
	default:
		return denied("permission denied: unsupported bash mode %q", mode)
	}
}

func commandNames(cmd string) []string {
	cmd = strings.NewReplacer("&&", ";", "||", ";", "|", ";").Replace(cmd)
	parts := strings.Split(cmd, ";")
	var out []string
	for _, part := range parts {
		name := firstCommandName(part)
		if name == "" || name == "cd" {
			continue
		}
		out = append(out, name)
	}
	return out
}

func firstCommandName(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	name := fields[0]
	for strings.Contains(name, "=") && len(fields) > 1 {
		fields = fields[1:]
		name = fields[0]
	}
	return filepath.Base(strings.Trim(name, "'\""))
}
