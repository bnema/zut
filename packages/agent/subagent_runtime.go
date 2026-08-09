package agent

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

// subagentRuntime owns the manager-facing runtime assembled around a host
// session. Keeping this state together is important: every injected manager
// tool must use the same supervisor, model/provider selection, profile root,
// policy, callbacks, and shutdown lifecycle.
type subagentRuntime struct {
	supervisor *subagents.Supervisor
	args       Args
	root       string

	provider    string
	model       string
	reasoning   string
	baseURL     string
	insecureTLS bool
	fastMode    bool
	repoRoot    string

	activeProvider  func() string
	activeModel     func() string
	activeReasoning func() string

	apiKey          string
	apiKeyProvider  string
	webSearchPolicy subagents.WebSearchPolicy
	webSearchGuard  *webSearchSessionGuard

	onSpawned       func(*subagents.Agent, string)
	onResumed       func(*subagents.Agent, string)
	beforeResumed   func(*subagents.Agent, string) func()
	onStopRequested func(*subagents.Agent)

	settingsMu sync.RWMutex
	closeMu    sync.Mutex
	closed     bool
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
	Context  context.Context
	Args     Args
	Root     string
	RepoRoot string

	Provider    string
	Model       string
	Reasoning   string
	BaseURL     string
	InsecureTLS bool
	FastMode    bool

	APIKey          string
	Policy          subagents.SubagentPolicy
	WebSearchPolicy subagents.WebSearchPolicy
	WebSearchGuard  *webSearchSessionGuard
	ActiveProvider  func() string
	ActiveModel     func() string
	ActiveReasoning func() string
	NewRunner       func(*subagents.Agent) subagents.Runner

	OnSpawned       func(*subagents.Agent, string)
	OnResumed       func(*subagents.Agent, string)
	BeforeResumed   func(*subagents.Agent, string) func()
	OnStopRequested func(*subagents.Agent)
}

func newSubagentRuntime(cfg subagentRuntimeConfig) *subagentRuntime {
	if cfg.Context == nil {
		cfg.Context = context.Background()
	}
	rt := &subagentRuntime{
		args:            cfg.Args,
		root:            cfg.Root,
		provider:        strings.TrimSpace(cfg.Provider),
		model:           strings.TrimSpace(cfg.Model),
		reasoning:       strings.TrimSpace(cfg.Reasoning),
		baseURL:         strings.TrimSpace(cfg.BaseURL),
		insecureTLS:     cfg.InsecureTLS,
		fastMode:        cfg.FastMode,
		repoRoot:        cfg.RepoRoot,
		activeProvider:  cfg.ActiveProvider,
		activeModel:     cfg.ActiveModel,
		activeReasoning: cfg.ActiveReasoning,
		apiKey:          cfg.APIKey,
		webSearchPolicy: cfg.WebSearchPolicy,
		webSearchGuard:  cfg.WebSearchGuard,
		onSpawned:       cfg.OnSpawned,
		onResumed:       cfg.OnResumed,
		beforeResumed:   cfg.BeforeResumed,
		onStopRequested: cfg.OnStopRequested,
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
	if rt.apiKey != "" {
		// A launch-time API key belongs to the provider selected at startup. It
		// must not follow the live provider callback through a later transition.
		rt.apiKeyProvider = canonicalProvider(rt.provider)
	}

	// Packaged agents have an explicit capability ceiling. Until worker
	// capability propagation exists, they cannot expose delegation tools or a
	// fresh unrestricted supervisor.
	if cfg.Args.PermissionSet == nil {
		root := rt.root
		if strings.TrimSpace(root) == "" {
			root = filepath.Join(ZutHome(), "subagents")
			rt.root = root
		}
		rt.supervisor = subagents.New(subagents.Config{
			Context:           cfg.Context,
			Root:              root,
			RepoRoot:          cfg.RepoRoot,
			Provider:          rt.currentProvider(),
			FastMode:          cfg.FastMode,
			WebSearchPolicy:   cfg.WebSearchPolicy,
			BaseURL:           cfg.BaseURL,
			InsecureTLS:       cfg.InsecureTLS,
			NewRunner:         cfg.NewRunner,
			Policy:            cfg.Policy,
			ResolveCredential: rt.resolveCredential,
		})
	}
	return rt
}

func (rt *subagentRuntime) Supervisor() *subagents.Supervisor {
	if rt == nil {
		return nil
	}
	return rt.supervisor
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
	if rt.supervisor != nil {
		rt.supervisor.SetProvider(providerID)
	}
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
	if rt.supervisor != nil {
		rt.supervisor.SetProviderSettings(baseURL, insecureTLS)
	}
}

func (rt *subagentRuntime) SetFastMode(fastMode bool) {
	if rt == nil {
		return
	}
	rt.settingsMu.Lock()
	rt.fastMode = fastMode
	rt.settingsMu.Unlock()
	if rt.supervisor != nil {
		rt.supervisor.SetFastMode(fastMode)
	}
}

func (rt *subagentRuntime) SetRepoRoot(repoRoot string) {
	if rt == nil {
		return
	}
	rt.settingsMu.Lock()
	rt.repoRoot = repoRoot
	rt.settingsMu.Unlock()
	if rt.supervisor != nil {
		rt.supervisor.SetRepoRoot(repoRoot)
	}
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
	if rt.supervisor != nil {
		rt.supervisor.SetProvider(snapshot.provider)
		rt.supervisor.SetProviderSettings(snapshot.baseURL, snapshot.insecureTLS)
		rt.supervisor.SetFastMode(snapshot.fastMode)
		rt.supervisor.SetRepoRoot(snapshot.repoRoot)
		rt.supervisor.SetWebSearchPolicy(snapshot.webSearchPolicy)
	}
}

func (rt *subagentRuntime) SetActiveSession(sessionID string) {
	if rt != nil && rt.supervisor != nil {
		rt.supervisor.SetActiveSession(sessionID)
	}
}

func (rt *subagentRuntime) SetWebSearchPolicy(policy subagents.WebSearchPolicy) {
	if rt == nil {
		return
	}
	rt.settingsMu.Lock()
	rt.webSearchPolicy = policy
	rt.settingsMu.Unlock()
	if rt.supervisor != nil {
		rt.supervisor.SetWebSearchPolicy(policy)
	}
}

func (rt *subagentRuntime) resolveCredential(ctx context.Context, providerID string) (subagents.Credential, error) {
	childProvider := canonicalProvider(providerID)
	if childProvider == "" {
		childProvider = canonicalProvider(rt.currentProvider())
	}
	explicit := ""
	// A launch-time API key is scoped to the provider selected at startup.
	// Never send it to an explicitly different child provider; resolve that
	// provider's own credential instead.
	if rt.apiKey != "" && childProvider == rt.apiKeyProvider {
		explicit = rt.apiKey
	}
	if childProvider == "ollama" && explicit == "" {
		return subagents.Credential{Value: "ollama", Method: "apikey"}, nil
	}
	credential, method, accountID, err := ResolveCredentialFullContext(ctx, childProvider, explicit)
	if err != nil {
		// Providers backed only by a local endpoint do not need a key. Leave
		// stdin untouched and let the child resolve that endpoint.
		if !CredentialAvailable(childProvider) {
			return subagents.Credential{}, nil
		}
		return subagents.Credential{}, err
	}
	return subagents.Credential{Value: credential, Method: method, AccountID: accountID}, nil
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
	if reg == nil || rt == nil || rt.supervisor == nil || !autoSubagentsAnyToolAllowed(rt.args) {
		return reg
	}
	if autoSubagentsToolAllowed(rt.args) {
		spawn := &tools.SubagentSpawnTool{
			Supervisor: rt.supervisor,
			Enabled:    func() bool { return true },
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
			OnSpawned:       rt.onSpawned,
		}
		reg[spawn.Name()] = spawn
	}
	if autoSubagentsStatusToolAllowed(rt.args) {
		status := &tools.SubagentStatusTool{
			Supervisor: rt.supervisor,
			Enabled:    func() bool { return true },
		}
		reg[status.Name()] = status
	}
	if autoSubagentsStopToolAllowed(rt.args) {
		stop := &tools.SubagentStopTool{
			Supervisor:      rt.supervisor,
			Enabled:         func() bool { return true },
			OnStopRequested: rt.onStopRequested,
		}
		reg[stop.Name()] = stop
	}
	if autoSubagentsResumeToolAllowed(rt.args) {
		resume := &tools.SubagentResumeTool{
			Supervisor:    rt.supervisor,
			Enabled:       func() bool { return true },
			BeforeResumed: rt.beforeResumed,
			OnResumed:     rt.onResumed,
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
	// Keep the supervisor's capability policy in lockstep with the resolved
	// parent registry. This is especially important after entry.pre refreshes
	// permissions: workers launched after the refresh must not retain the old
	// parent allow decision.
	if rt != nil {
		rt.SetWebSearchPolicy(webSearchPolicyForRegistry(policy, reg))
	}
	if policy != subagents.WebSearchAllow {
		delete(reg, "web_search")
	}
	return rt.PrepareRegistry(reg)
}

func (rt *subagentRuntime) Close(ctx context.Context) error {
	if rt == nil || rt.supervisor == nil {
		return nil
	}
	rt.closeMu.Lock()
	defer rt.closeMu.Unlock()
	if rt.closed {
		return nil
	}
	if err := rt.supervisor.StopAllContext(ctx); err != nil {
		return err
	}
	rt.closed = true
	return nil
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
		MaxConcurrent:          cfg.MaxConcurrent,
		MaxConcurrentPerParent: cfg.MaxConcurrentPerParent,
		QueueTimeout:           parseDuration(cfg.QueueTimeout),
		DefaultTimeout:         parseDuration(cfg.DefaultTimeout),
		MaxTurns:               cfg.MaxTurns,
		MaxOutputBytes:         cfg.MaxOutputBytes,
		MaxOutputLines:         cfg.MaxOutputLines,
		AllowedTools:           append([]string(nil), cfg.AllowedTools...),
		AllowedRoots:           append([]string(nil), cfg.AllowedRoots...),
		HeartbeatInterval:      parseDuration(cfg.HeartbeatInterval),
		IdleTimeout:            parseDuration(cfg.IdleTimeout),
		ReconnectTimeout:       parseDuration(cfg.ReconnectTimeout),
		CancelGracePeriod:      parseDuration(cfg.CancelGracePeriod),
	}
}
