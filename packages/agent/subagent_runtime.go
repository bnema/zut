package agent

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/google/uuid"
)

// subagentRuntime owns the manager-facing runtime assembled around a host
// session. Every injected manager tool shares the same resident manager,
// model/provider selection, profile root, policy, and shutdown lifecycle.
type subagentRuntime struct {
	resident *subagents.ResidentManager
	args     Args
	root     string

	provider string
	// credentialProvider is immutable: an explicit CLI key belongs to the
	// provider resolved when this runtime was assembled, not to later UI
	// provider selections.
	credentialProvider string
	model              string
	reasoning          string
	baseURL            string
	insecureTLS        bool
	fastMode           bool
	repoRoot           string

	activeProvider  func() string
	activeModel     func() string
	activeReasoning func() string

	webSearchPolicy subagents.WebSearchPolicy
	policy          subagents.SubagentPolicy
	activeSession   string
	webSearchGuard  *webSearchSessionGuard

	onResidentSpawned func(subagents.ResidentChildSpec, string)
	settingsMu        sync.RWMutex
	closeMu           sync.Mutex
	closed            bool
}

type subagentRuntimeConfiguration struct {
	provider        string
	model           string
	baseURL         string
	insecureTLS     bool
	fastMode        bool
	repoRoot        string
	webSearchPolicy subagents.WebSearchPolicy
}

// subagentRuntimeConfig supplies the host-owned settings and seams needed to
// assemble a subagentRuntime. The active selection callbacks are optional;
// when omitted, the initial Provider/Model/Reasoning values are used.
type subagentRuntimeConfig struct {
	Args     Args
	Root     string
	RepoRoot string

	Provider    string
	Model       string
	Reasoning   string
	BaseURL     string
	InsecureTLS bool
	FastMode    bool

	Policy             subagents.SubagentPolicy
	WebSearchPolicy    subagents.WebSearchPolicy
	WebSearchGuard     *webSearchSessionGuard
	ActiveProvider     func() string
	ActiveModel        func() string
	ActiveReasoning    func() string
	ResidentCompletion func(subagents.ResidentCompletion)
	OnResidentSpawned  func(subagents.ResidentChildSpec, string)
}

func newSubagentRuntime(cfg subagentRuntimeConfig) *subagentRuntime {
	rt := &subagentRuntime{
		args:               cfg.Args,
		root:               cfg.Root,
		provider:           strings.TrimSpace(cfg.Provider),
		credentialProvider: strings.TrimSpace(cfg.Provider),
		model:              strings.TrimSpace(cfg.Model),
		reasoning:          strings.TrimSpace(cfg.Reasoning),
		baseURL:            strings.TrimSpace(cfg.BaseURL),
		insecureTLS:        cfg.InsecureTLS,
		fastMode:           cfg.FastMode,
		repoRoot:           cfg.RepoRoot,
		activeProvider:     cfg.ActiveProvider,
		activeModel:        cfg.ActiveModel,
		activeReasoning:    cfg.ActiveReasoning,
		webSearchPolicy:    cfg.WebSearchPolicy,
		policy:             cfg.Policy,
		webSearchGuard:     cfg.WebSearchGuard,
		onResidentSpawned:  cfg.OnResidentSpawned,
	}
	if rt.activeProvider == nil {
		rt.activeProvider = func() string {
			rt.settingsMu.RLock()
			defer rt.settingsMu.RUnlock()
			return rt.provider
		}
	}
	if rt.activeModel == nil {
		rt.activeModel = func() string {
			rt.settingsMu.RLock()
			defer rt.settingsMu.RUnlock()
			return rt.model
		}
	}
	if rt.activeReasoning == nil {
		rt.activeReasoning = func() string {
			rt.settingsMu.RLock()
			defer rt.settingsMu.RUnlock()
			return rt.reasoning
		}
	}
	// Packaged agents have an explicit capability ceiling and cannot delegate.
	if cfg.Args.PermissionSet == nil {
		root := rt.root
		if strings.TrimSpace(root) == "" {
			root = filepath.Join(ZutHome(), "subagents")
			rt.root = root
		}
		rt.resident = subagents.NewResidentManagerWithPolicy(root, cfg.Policy, func(spec subagents.ResidentChildSpec, journal *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
			return newResidentChildRunner(residentChildArgs(rt.args, rt.credentialProvider, spec), spec, journal)
		})
		rt.resident.SetCompletionObserver(cfg.ResidentCompletion)
		rt.resident.SetAcceptedObserver(cfg.OnResidentSpawned)
	}
	return rt
}

// ResidentManager returns the in-process runtime.
func (rt *subagentRuntime) ResidentManager() *subagents.ResidentManager {
	if rt == nil {
		return nil
	}
	return rt.resident
}

func (rt *subagentRuntime) residentManagerForTools() *subagents.ResidentManager {
	if rt == nil {
		return nil
	}
	return rt.resident
}

func (rt *subagentRuntime) currentProvider() string {
	if rt == nil || rt.activeProvider == nil {
		return ""
	}
	return strings.TrimSpace(rt.activeProvider())
}

func (rt *subagentRuntime) currentModel() string {
	if rt == nil || rt.activeModel == nil {
		return ""
	}
	return strings.TrimSpace(rt.activeModel())
}

func (rt *subagentRuntime) SetModel(model string) {
	if rt == nil {
		return
	}
	rt.settingsMu.Lock()
	rt.model = strings.TrimSpace(model)
	rt.settingsMu.Unlock()
}

func (rt *subagentRuntime) SetReasoning(reasoning string) {
	if rt == nil {
		return
	}
	rt.settingsMu.Lock()
	rt.reasoning = strings.TrimSpace(reasoning)
	rt.settingsMu.Unlock()
}

func (rt *subagentRuntime) currentReasoning() string {
	if rt == nil || rt.activeReasoning == nil {
		return ""
	}
	return strings.TrimSpace(rt.activeReasoning())
}

func (rt *subagentRuntime) SetProvider(providerID string) {
	if rt == nil {
		return
	}
	providerID = strings.TrimSpace(providerID)
	rt.settingsMu.Lock()
	rt.provider = providerID
	rt.settingsMu.Unlock()
}

func (rt *subagentRuntime) SetProviderSettings(baseURL string, insecureTLS bool) {
	if rt == nil {
		return
	}
	baseURL = strings.TrimSpace(baseURL)
	rt.settingsMu.Lock()
	rt.baseURL = baseURL
	rt.insecureTLS = insecureTLS
	rt.settingsMu.Unlock()
}

func (rt *subagentRuntime) SetFastMode(fastMode bool) {
	if rt == nil {
		return
	}
	rt.settingsMu.Lock()
	rt.fastMode = fastMode
	rt.settingsMu.Unlock()
}

func (rt *subagentRuntime) SetRepoRoot(repoRoot string) {
	if rt == nil {
		return
	}
	rt.settingsMu.Lock()
	rt.repoRoot = repoRoot
	rt.settingsMu.Unlock()
}

// snapshotConfiguration captures the runtime settings changed while an agent
// candidate is being built. Transition callers restore it when candidate
// preparation fails before the new agent is committed.
func (rt *subagentRuntime) snapshotConfiguration() subagentRuntimeConfiguration {
	if rt == nil {
		return subagentRuntimeConfiguration{}
	}
	rt.settingsMu.RLock()
	defer rt.settingsMu.RUnlock()
	return subagentRuntimeConfiguration{
		provider:        rt.provider,
		model:           rt.model,
		baseURL:         rt.baseURL,
		insecureTLS:     rt.insecureTLS,
		fastMode:        rt.fastMode,
		repoRoot:        rt.repoRoot,
		webSearchPolicy: rt.webSearchPolicy,
	}
}

func (rt *subagentRuntime) restoreConfiguration(snapshot subagentRuntimeConfiguration) {
	if rt == nil {
		return
	}
	rt.settingsMu.Lock()
	rt.provider = snapshot.provider
	rt.model = snapshot.model
	rt.baseURL = snapshot.baseURL
	rt.insecureTLS = snapshot.insecureTLS
	rt.fastMode = snapshot.fastMode
	rt.repoRoot = snapshot.repoRoot
	rt.webSearchPolicy = snapshot.webSearchPolicy
	rt.settingsMu.Unlock()
}

func (rt *subagentRuntime) SetActiveSession(sessionID string) {
	if rt == nil {
		return
	}
	rt.settingsMu.Lock()
	rt.activeSession = strings.TrimSpace(sessionID)
	rt.settingsMu.Unlock()
}

func (rt *subagentRuntime) buildResidentChildSpec(_ context.Context, request tools.ResidentSpawnRequest, catalogue core.Registry) (subagents.ResidentChildSpec, error) {
	if rt == nil {
		return subagents.ResidentChildSpec{}, errors.New("resident runtime is unavailable")
	}
	rt.settingsMu.RLock()
	activeProvider := strings.TrimSpace(rt.provider)
	providerID, model, reasoning := activeProvider, rt.model, rt.reasoning
	baseURL, insecureTLS := rt.baseURL, rt.insecureTLS
	workspace, parentSession := rt.repoRoot, rt.activeSession
	fastMode, policy := rt.fastMode, rt.policy
	rt.settingsMu.RUnlock()
	if strings.TrimSpace(request.Provider) != "" {
		providerID = strings.TrimSpace(request.Provider)
	}
	if strings.TrimSpace(request.Model) != "" {
		model = request.Model
	}
	if strings.TrimSpace(request.Reasoning) != "" {
		reasoning = request.Reasoning
	}
	if request.FastMode != nil {
		fastMode = *request.FastMode
	}
	if strings.TrimSpace(providerID) == "" || strings.TrimSpace(model) == "" {
		return subagents.ResidentChildSpec{}, errors.New("resident child needs a provider and model")
	}
	if providerID != activeProvider {
		// A custom endpoint and TLS exception belong to the provider that
		// declared them. Never carry a parent route into an explicitly
		// selected provider: Resolve must use that provider's own settings.
		baseURL, insecureTLS = "", false
	}
	allTools := make([]string, 0, len(catalogue))
	for name := range catalogue {
		switch name {
		case tools.SubagentSpawnToolName, tools.SubagentStatusToolName, tools.SubagentStopToolName, tools.SubagentResumeToolName, "update_goal":
			continue
		}
		allTools = append(allTools, name)
	}
	sort.Strings(allTools)
	permitted := func(name string) bool { return policy.AllowsTool(name) }
	var childTools []string
	var err error
	if request.Profile == nil || !request.Profile.ToolsDeclared {
		for _, name := range allTools {
			if permitted(name) {
				childTools = append(childTools, name)
			}
		}
	} else {
		childTools, err = subagents.ResolveProfileTools(request.Profile, allTools, permitted)
		if err != nil {
			return subagents.ResidentChildSpec{}, err
		}
	}
	workspaceMode := request.WorkspaceMode
	if workspaceMode == "" {
		workspaceMode = subagents.WorkspaceShared
	}
	spec := subagents.ResidentChildSpec{
		ID: uuid.NewString(), SessionID: uuid.NewString(), ParentSessionID: parentSession,
		Provider: strings.TrimSpace(providerID), BaseURL: strings.TrimSpace(baseURL), InsecureTLS: insecureTLS, Model: strings.TrimSpace(model),
		Reasoning: strings.TrimSpace(reasoning), FastMode: fastMode, Tools: childTools,
		RepositoryRoot: workspace, Workspace: workspace, WorkspaceMode: workspaceMode, Required: request.Required,
	}
	if request.Profile != nil {
		spec.Profile = request.Profile.Name
		spec.SystemPrompt = request.Profile.SystemPrompt
		spec.SystemPromptMode = request.Profile.SystemPromptMode
		spec.InheritProjectContext = request.Profile.InheritProjectContext
		spec.InheritSkills = request.Profile.InheritSkills
	}
	return spec, nil
}

func (rt *subagentRuntime) SetWebSearchPolicy(policy subagents.WebSearchPolicy) {
	if rt == nil {
		return
	}
	rt.settingsMu.Lock()
	rt.webSearchPolicy = policy
	rt.settingsMu.Unlock()
}

func (rt *subagentRuntime) resolveSubagent(name string) (*subagents.Profile, error) {
	if rt == nil {
		return nil, nil
	}
	rt.settingsMu.RLock()
	repoRoot := rt.repoRoot
	rt.settingsMu.RUnlock()
	return findSubagentProfile(repoRoot, name)
}

// InjectTools adds the canonical manager tools allowed by the host launch
// policy. It deliberately mutates the supplied registry, matching the normal
// interactive registry assembly path.
func (rt *subagentRuntime) InjectTools(reg core.Registry) core.Registry {
	if reg == nil || rt == nil || rt.resident == nil || !autoSubagentsAnyToolAllowed(rt.args) {
		return reg
	}
	if autoSubagentsToolAllowed(rt.args) {
		spawn := &tools.SubagentSpawnTool{
			ResidentManager: rt.residentManagerForTools(),
			Enabled:         func() bool { return true },
			DefaultModel: func() string {
				return rt.currentModel()
			},
			DefaultProvider: func() string {
				return rt.currentProvider()
			},
			DefaultReasoning: func() string {
				return rt.currentReasoning()
			},
			ResolveSubagent: rt.resolveSubagent,
			BuildResidentSpec: func(ctx context.Context, request tools.ResidentSpawnRequest) (subagents.ResidentChildSpec, error) {
				return rt.buildResidentChildSpec(ctx, request, reg)
			},
			OnResidentSpawned: rt.onResidentSpawned,
		}
		reg[spawn.Name()] = spawn
	}
	if autoSubagentsStatusToolAllowed(rt.args) {
		status := &tools.SubagentStatusTool{
			ResidentManager: rt.residentManagerForTools(),
			Enabled:         func() bool { return true },
		}
		reg[status.Name()] = status
	}
	if autoSubagentsStopToolAllowed(rt.args) {
		stop := &tools.SubagentStopTool{
			ResidentManager: rt.residentManagerForTools(),
			Enabled:         func() bool { return true },
		}
		reg[stop.Name()] = stop
	}
	if autoSubagentsResumeToolAllowed(rt.args) {
		resume := &tools.SubagentResumeTool{
			ResidentManager: rt.residentManagerForTools(),
			Enabled:         func() bool { return true },
		}
		reg[resume.Name()] = resume
	}
	return reg
}

func (rt *subagentRuntime) PrepareRegistry(reg core.Registry) core.Registry {
	reg = rt.InjectTools(reg)
	if rt != nil && rt.webSearchGuard != nil {
		return rt.webSearchGuard.wrapRegistry(reg)
	}
	return reg
}

func (rt *subagentRuntime) PrepareResolvedRegistry(reg core.Registry, policy subagents.WebSearchPolicy) core.Registry {
	if rt != nil {
		rt.SetWebSearchPolicy(webSearchPolicyForRegistry(policy, reg))
	}
	if policy != subagents.WebSearchAllow {
		delete(reg, "web_search")
	}
	return rt.PrepareRegistry(reg)
}

func (rt *subagentRuntime) Close(ctx context.Context) error {
	if rt == nil {
		return nil
	}
	rt.closeMu.Lock()
	defer rt.closeMu.Unlock()
	if rt.closed {
		return nil
	}
	var err error
	if rt.resident != nil {
		err = errors.Join(err, rt.resident.Close(ctx))
	}
	rt.closed = true
	return err
}

func subagentPolicyFromConfig(cfg SubagentsConfig) subagents.SubagentPolicy {
	parseDuration := func(value string) time.Duration {
		if strings.TrimSpace(value) == "" {
			return 0
		}
		parsed, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || parsed <= 0 {
			return 0
		}
		return parsed
	}
	return subagents.SubagentPolicy{
		MaxConcurrent: cfg.MaxConcurrent,
		QueueTimeout:  parseDuration(cfg.QueueTimeout),
		AllowedTools:  append([]string(nil), cfg.AllowedTools...),
		AllowedRoots:  append([]string(nil), cfg.AllowedRoots...),
	}
}
