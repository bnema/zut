package modes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/provider"
)

func (i *Interactive) runSubagents(ctx context.Context, args []string) {
	if i.residentPort() == nil {
		i.mu.Lock()
		i.statusErr = i.autoSubagentsUnavailableHint()
		i.statusOK = ""
		i.mu.Unlock()
		return
	}
	i.runResidentSubagents(ctx, args)
}

// runResidentSubagents is the production slash-command path. The list/editor
// migration is deliberately kept separate from the legacy process dashboard;
// details open a structured journal-backed child session immediately.
func (i *Interactive) runResidentSubagents(ctx context.Context, args []string) {
	sub := ""
	rest := ""
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
		if len(args) > 1 {
			rest = strings.TrimSpace(strings.Join(args[1:], " "))
		}
	}
	switch sub {
	case "", "list", "ls", "ps":
		i.mu.Lock()
		if i.residentSubagentsDialog == nil {
			i.residentSubagentsDialog = newResidentSubagentsDialog(&i.mu, i.invalidate)
		}
		i.residentSubagentsDialog.Open(i.port)
		i.mu.Unlock()
		i.invalidate()
	case "new", "spawn":
		i.spawnResidentSubagent(rest)
	case "logs", "log", "view":
		if rest == "" {
			i.subagentsStatus("", "/subagents logs <id>: missing id")
			return
		}
		i.openResidentChildSession(rest)
	case "kill", "stop":
		if rest == "" {
			i.subagentsStatus("", "/subagents kill <id>: missing id")
			return
		}
		i.residentActionAsync(ctx, "kill", session.ResidentCommand{Op: "stop", ChildID: rest}, "stopped "+rest)
	case "send", "prompt", "msg", "resume":
		id, prompt := splitIDAndRest(rest)
		if id == "" || prompt == "" {
			i.subagentsStatus("", "/subagents "+sub+" <id> <prompt>: missing id or prompt")
			return
		}
		i.residentActionAsync(ctx, sub, session.ResidentCommand{Op: "resume", ChildID: id, Prompt: prompt}, "sent follow-up to "+id)
	default:
		i.subagentsStatus("", "/subagents: resident child sessions support list / new / logs / kill / send / resume")
	}
}

func (i *Interactive) openResidentChildSession(childID string) {
	i.mu.Lock()
	session := newResidentChildSession(i.port, childID, i.cfg.Theme)
	session.SetSummary(residentSummaryFor(i.portInfo.Residents, childID))
	previous := i.residentChildSession
	if i.residentSubagentsDialog != nil {
		i.residentSubagentsDialog.Close()
	}
	i.residentChildSession = session
	i.mu.Unlock()
	if previous != nil {
		previous.Close()
	}
	i.setResidentChildMouseReporting(true)
	i.invalidate()
	if session.BeginLoad() {
		go func() {
			err := session.LoadAll(200, i.invalidate)
			if session.FinishLoad(err) {
				i.reloadResidentChildSession(session)
			}
			i.invalidate()
		}()
	}
}

func (i *Interactive) spawnResidentSubagent(rest string) {
	if i.cfg.SpawnResident == nil {
		i.subagentsStatus("", "spawn: resident construction is unavailable")
		return
	}
	model, providerID, reasoning, profileName, task := parseSpawnFlags(rest)
	if task == "" {
		i.subagentsStatus("", "/subagents new <task>: missing task")
		return
	}
	var profile *subagents.Profile
	var fastModeOverride *bool
	if profileName != "" {
		if i.cfg.ResolveSubagent == nil {
			i.subagentsStatus("", "spawn: named subagent profiles are unavailable")
			return
		}
		resolved, err := i.cfg.ResolveSubagent(profileName)
		if err != nil || resolved == nil {
			if err != nil {
				i.subagentsStatus("", "spawn: "+err.Error())
			} else {
				i.subagentsStatus("", "spawn: unknown subagent profile "+profileName)
			}
			return
		}
		profile = resolved
		fastModeOverride = profile.FastMode
		if model == "" {
			providerID, model = profile.ModelSelection()
		}
		if reasoning == "" {
			reasoning = profile.Thinking
		}
	}
	if model == "" {
		model = i.cfg.Model
	}
	if providerID == "" {
		providerID = i.cfg.Provider
	}
	if reasoning == "" {
		reasoning = i.cfg.Reasoning
	}
	if reasoning != "" {
		var err error
		reasoning, err = normalizeSpawnReasoning(reasoning)
		if err != nil {
			i.subagentsStatus("", "spawn: "+err.Error())
			return
		}
	}
	childID, err := i.cfg.SpawnResident(context.Background(), tools.ResidentSpawnRequest{Task: task, Profile: profile, Model: model, Provider: providerID, Reasoning: reasoning, FastMode: fastModeOverride})
	if err != nil {
		i.subagentsStatus("", "spawn: "+err.Error())
		return
	}
	i.subagentsStatus("spawned "+childID, "")
}

// normalizeSpawnReasoning validates and canonicalizes the reasoning levels
// accepted by ParseArgs before a child process is launched.
func normalizeSpawnReasoning(value string) (string, error) {
	raw := strings.ToLower(strings.TrimSpace(value))
	switch raw {
	case "", "off":
		return raw, nil
	case "minimal", "minimum", "low", "medium", "high", "xhigh", "maximum", "max":
		return provider.NormalizeReasoning(raw), nil
	default:
		return "", fmt.Errorf("reasoning must be off|minimum|low|medium|high|xhigh|max")
	}
}

// parseSpawnFlags consumes leading profile/model/provider/reasoning flags
// using both separate-value and equals forms. Flags after the task remain
// part of the task text.
func parseSpawnFlags(s string) (model, provider, reasoning, subagent, task string) {
	fields := strings.Fields(s)
	i := 0
	for i < len(fields) {
		f := fields[i]
		switch {
		case f == "--agent":
			if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "--") {
				subagent = fields[i+1]
				i += 2
			} else {
				i++
			}
			continue
		case strings.HasPrefix(f, "--agent="):
			subagent = strings.TrimPrefix(f, "--agent=")
			i++
			continue
		case f == "--model":
			// Consume the flag even when no value follows so a
			// dangling "--model" doesn't leak into the task. The
			// caller surfaces "missing task" instead.
			if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "--") {
				model = fields[i+1]
				i += 2
			} else {
				i++
			}
			continue
		case strings.HasPrefix(f, "--model="):
			model = strings.TrimPrefix(f, "--model=")
			i++
			continue
		case f == "--provider":
			if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "--") {
				provider = fields[i+1]
				i += 2
			} else {
				i++
			}
			continue
		case strings.HasPrefix(f, "--provider="):
			provider = strings.TrimPrefix(f, "--provider=")
			i++
			continue
		case f == "--reasoning" || f == "--thinking":
			if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "--") {
				reasoning = fields[i+1]
				i += 2
			} else {
				i++
			}
			continue
		case strings.HasPrefix(f, "--reasoning="):
			reasoning = strings.TrimPrefix(f, "--reasoning=")
			i++
			continue
		case strings.HasPrefix(f, "--thinking="):
			reasoning = strings.TrimPrefix(f, "--thinking=")
			i++
			continue
		}
		break
	}
	task = strings.TrimSpace(strings.Join(fields[i:], " "))
	return
}

// splitIDAndRest splits "<id> <text...>" into (id, text). The text
// half preserves all whitespace after the first token so the agent
// receives the user's prompt verbatim (modulo a single trim of the
// boundary space). Returns ("", "") when s is empty so the caller
// can surface a missing-id error.
func splitIDAndRest(s string) (id, text string) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", ""
	}
	cut := strings.IndexAny(s, " \t")
	if cut < 0 {
		return s, ""
	}
	return s[:cut], strings.TrimLeft(s[cut+1:], " \t")
}

func truncateStatus(value string, max int) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\n", " ")
	if len([]rune(value)) <= max {
		return value
	}
	if max <= 3 {
		return string([]rune(value)[:max])
	}
	return string([]rune(value)[:max-3]) + "..."
}

func (i *Interactive) subagentsStatus(ok, errMsg string) {
	i.mu.Lock()
	i.statusOK = ok
	i.statusErr = errMsg
	i.mu.Unlock()
	i.invalidate()
}

// residentPort returns the attached session port, which owns residents.
func (i *Interactive) residentPort() session.Port {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.port
}

// hasResidentSubagents reads the session-published resident summaries; it
// never calls the port, so the key handler cannot block on the daemon.
func (i *Interactive) hasResidentSubagents() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.portInfo.Residents) > 0
}

// residentActionTimeout bounds one slash-command resident action.
const residentActionTimeout = 30 * time.Second

// residentActionAsync runs a resident action off the input goroutine and
// reports the outcome on the status line, like the child-view submit.
func (i *Interactive) residentActionAsync(ctx context.Context, label string, command session.ResidentCommand, ok string) {
	port := i.residentPort()
	if port == nil {
		i.subagentsStatus("", label+": "+i.autoSubagentsUnavailableHint())
		return
	}
	go func() {
		actionCtx, cancel := context.WithTimeout(ctx, residentActionTimeout)
		defer cancel()
		if err := port.ResidentAction(actionCtx, command); err != nil {
			i.subagentsStatus("", label+": "+err.Error())
			return
		}
		i.subagentsStatus(ok, "")
	}()
}
