package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	providerpkg "github.com/bnema/zut/packages/provider"
)

// advisorGuidance is the executor guidance shown whenever the advisor tool
// is registered.
//
//go:embed advisor_prompt.txt
var advisorGuidance string

// advisorSystemAddendum returns the advisor guidance when reg holds the
// advisor tool.
func advisorSystemAddendum(reg core.Registry) string {
	if _, ok := reg[tools.AdvisorToolName]; !ok {
		return ""
	}
	return "[advisor]\n" + strings.TrimSpace(advisorGuidance)
}

// advisorToolAllowed gates the advisor to unrestricted main sessions with the
// advisor enabled. Packaged agents (PermissionSet, named agents, zutfiles),
// side chats and SDK runtimes (NoAdvisor) are excluded because the tool sends
// the transcript to another provider. Native mode requires this too.
func advisorToolAllowed(args Args, cfg Config) bool {
	if !cfg.AdvisorSettings().Enabled || args.NoAdvisor || args.NoTools || args.PermissionSet != nil ||
		args.ResidentChild || args.AgentName != "" || args.zutfile != nil {
		return false
	}
	if len(args.Tools) == 0 {
		return !args.ToolsSet
	}
	return toolListContains(args.Tools, tools.AdvisorToolName)
}

// maybeAddAdvisorTool registers the advisor tool. The tool reloads config on
// each call, so an enabled advisor without a model still registers and
// reports the missing model to the executor.
func maybeAddAdvisorTool(reg core.Registry, args Args, cfg Config) {
	if reg == nil || !advisorToolAllowed(args, cfg) {
		return
	}
	getenv := argsGetenv(args)
	reg[tools.AdvisorToolName] = &tools.AdvisorTool{
		Target: func(ctx context.Context) (tools.AdvisorTarget, error) {
			return resolveAdvisorTarget(ctx, getenv)
		},
	}
}

// bindAdvisorTool attaches ag's transcript to the advisor tool in reg, if any.
func bindAdvisorTool(reg core.Registry, ag *core.Agent) {
	adv, ok := reg[tools.AdvisorToolName].(*tools.AdvisorTool)
	if !ok || ag == nil {
		return
	}
	adv.Bind(func() tools.AdvisorSnapshot {
		system, current := ag.PromptConfig()
		return tools.AdvisorSnapshot{System: system, Tools: current.Specs(), Messages: ag.Messages()}
	})
}

// resolveAdvisorTarget loads the current advisor settings and builds a
// client for the configured model.
func resolveAdvisorTarget(ctx context.Context, getenv func(string) string) (tools.AdvisorTarget, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return tools.AdvisorTarget{}, fmt.Errorf("load advisor config: %w", err)
	}
	settings := cfg.AdvisorSettings()
	if !settings.Enabled {
		return tools.AdvisorTarget{}, errors.New("advisor is disabled; run /advisor on")
	}
	if settings.Model == "" {
		return tools.AdvisorTarget{}, tools.ErrNoAdvisorModel
	}
	spec, err := ParseAdvisorModelSpec(settings.Model)
	if err != nil {
		return tools.AdvisorTarget{}, err
	}
	client, model, err := newAdvisorClient(ctx, spec, getenv)
	if err != nil {
		return tools.AdvisorTarget{}, err
	}
	return tools.AdvisorTarget{Client: client, Model: model, Reasoning: spec.Reasoning, MaxTokens: settings.MaxTokens}, nil
}

// resolveAdvisorEndpoint resolves credential and endpoint for an advisor
// model with the same rules as the executor (keyless custom providers,
// ollama, llama.cpp, insecure custom endpoints), without a full Resolve. It
// is used both when /advisor validates a model and when the tool calls it.
func resolveAdvisorEndpoint(ctx context.Context, spec AdvisorModelSpec, getenv func(string) string) (Resolved, providerpkg.Model, error) {
	model, err := providerpkg.FindModel(spec.Provider, spec.Model)
	if err != nil {
		return Resolved{}, providerpkg.Model{}, err
	}
	var custom *providerpkg.CustomProviderConfig
	if c, ok := providerpkg.CustomProviders()[spec.Provider]; ok {
		custom = &c
		if model.BaseURL == "" {
			model.BaseURL = c.BaseURL
		}
	}
	cfg, _ := LoadConfig()
	discovered := model.Source == "live" && isDiscoverableCustomProvider(spec.Provider)
	ec := resolveEndpointCredential(ctx, spec.Provider, "", getenv)
	ep, err := resolveEndpoint(ctx, endpointInput{
		provider:        spec.Provider,
		model:           model,
		explicitBaseURL: (model.Source == "user" || discovered) && model.BaseURL != "",
		insecure:        cfg.Insecure,
		custom:          custom,
		getenv:          getenv,
	}, &ec)
	if err != nil {
		return Resolved{}, providerpkg.Model{}, fmt.Errorf("advisor endpoint for %q: %w", spec.Provider, err)
	}
	r := Resolved{
		Provider:             spec.Provider,
		Model:                model.ID,
		Credential:           ec.cred,
		AuthMethod:           ec.method,
		AccountID:            ec.accountID,
		BaseURL:              ep.baseURL,
		InsecureTLS:          ep.insecureTLS,
		credentialOptional:   ep.credentialOptional,
		customProviderConfig: custom,
	}
	if ec.err != nil {
		return Resolved{}, providerpkg.Model{}, errNoAdvisorCredential(spec.Provider, ec.err)
	}
	if !r.HasCredential() {
		return Resolved{}, providerpkg.Model{}, errNoAdvisorCredential(spec.Provider, nil)
	}
	return r, model, nil
}

// newAdvisorClient builds a provider client for the advisor model. It returns
// an error instead of panicking when the provider has no usable credential.
func newAdvisorClient(ctx context.Context, spec AdvisorModelSpec, getenv func(string) string) (providerpkg.Client, providerpkg.Model, error) {
	r, model, err := resolveAdvisorEndpoint(ctx, spec, getenv)
	if err != nil {
		return nil, providerpkg.Model{}, err
	}
	return r.NewClient(), model, nil
}

// errNoAdvisorCredential reports a provider without a usable credential.
// cause is the lookup error (never contains the secret), or nil.
func errNoAdvisorCredential(provider string, cause error) error {
	if cause == nil {
		return fmt.Errorf("no credential for advisor provider %q; log in or set its API key", provider)
	}
	return fmt.Errorf("no credential for advisor provider %q; log in or set its API key: %w", provider, cause)
}

// advisorSettingsStore is the session.AdvisorStore backend: config.json
// plus a credential check in the session environment.
type advisorSettingsStore struct {
	getenv func(string) string
}

func advisorStatusFromConfig(cfg Config) session.AdvisorStatus {
	s := cfg.AdvisorSettings()
	return session.AdvisorStatus{Enabled: s.Enabled, Model: s.Model, Mode: s.Mode, MaxTokens: s.MaxTokens}
}

func (advisorSettingsStore) AdvisorStatus() (session.AdvisorStatus, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return session.AdvisorStatus{}, err
	}
	return advisorStatusFromConfig(cfg), nil
}

// SetAdvisor validates the whole change, then applies it in one config
// write. Setting a model enables the advisor unless Enabled says otherwise.
func (s advisorSettingsStore) SetAdvisor(change session.AdvisorChange) (session.AdvisorStatus, error) {
	if change.Model == nil && change.Enabled == nil {
		return s.AdvisorStatus()
	}
	var model string
	if change.Model != nil {
		spec, err := ParseAdvisorModelSpec(*change.Model)
		if err != nil {
			return session.AdvisorStatus{}, err
		}
		getenv := s.getenv
		if getenv == nil {
			getenv = os.Getenv
		}
		if _, _, err := resolveAdvisorEndpoint(context.Background(), spec, getenv); err != nil {
			return session.AdvisorStatus{}, err
		}
		model = spec.String()
	}
	var status session.AdvisorStatus
	err := updateConfig(func(c *Config) error {
		next := AdvisorConfig{}
		if c.Advisor != nil {
			next = *c.Advisor
		}
		if change.Model != nil {
			next.Model = model
			next.Enabled = true
		}
		if change.Enabled != nil {
			next.Enabled = *change.Enabled
		}
		if next.Enabled && strings.TrimSpace(next.Model) == "" {
			return tools.ErrNoAdvisorModel
		}
		if c.Advisor == nil && !next.Enabled && next.Model == "" {
			status = advisorStatusFromConfig(*c)
			return errConfigUnchanged
		}
		c.Advisor = &next
		status = advisorStatusFromConfig(*c)
		return nil
	})
	if err != nil {
		return session.AdvisorStatus{}, err
	}
	return status, nil
}

// decideAdvisorMode returns the server advisor for a native-capable executor,
// or nil for the client tool (or no advisor). available reports whether the
// session admits the advisor at all (the client tool was registered). Native
// mode needs auto mode, a first-party Anthropic executor and advisor, and a
// pair the Anthropic advisor tool accepts.
func decideAdvisorMode(available bool, s AdvisorSettings, executorProvider, executorModel string) *providerpkg.NativeAdvisor {
	if !available || !s.Enabled || s.Mode != AdvisorModeAuto || s.Model == "" || executorProvider != "anthropic" {
		return nil
	}
	spec, err := ParseAdvisorModelSpec(s.Model)
	if err != nil || spec.Provider != "anthropic" || !providerpkg.AnthropicAdvisorCompatible(executorModel, spec.Model) {
		return nil
	}
	return &providerpkg.NativeAdvisor{Model: spec.Model, MaxTokens: s.MaxTokens}
}

// applyAdvisorMode removes the client advisor tool from reg when the native
// advisor applies, and returns the server advisor to set on the agent (nil
// for client mode or no advisor). The advisor prompt addendum is kept in both
// modes because the native tool is also named "advisor".
func applyAdvisorMode(reg core.Registry, s AdvisorSettings, executorProvider, executorModel string) *providerpkg.NativeAdvisor {
	_, available := reg[tools.AdvisorToolName]
	server := decideAdvisorMode(available, s, executorProvider, executorModel)
	if server != nil {
		delete(reg, tools.AdvisorToolName)
	}
	return server
}

// installServerAdvisor sets the decided native advisor on ag and guards it
// per request: config.json is shared by every session, so the advisor is
// dropped as soon as the persisted settings no longer match the decision
// (disabled, other model, mode or max_tokens). The session resyncs its tools
// at the next idle point (turn end) and decides again.
func installServerAdvisor(ag *core.Agent, server *providerpkg.NativeAdvisor, decided AdvisorSettings) {
	ag.SetServerAdvisor(server)
	if server == nil {
		ag.SetServerAdvisorFunc(nil)
		return
	}
	adv := *server
	ag.SetServerAdvisorFunc(func() *providerpkg.NativeAdvisor {
		cfg, err := LoadConfig()
		if err != nil || cfg.AdvisorSettings() != decided {
			return nil
		}
		out := adv
		return &out
	})
}
