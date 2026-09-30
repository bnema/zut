package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

type copilotRequestKey struct{}
type copilotRequestMetadata struct {
	initiator string
	vision    bool
}

type copilotClient struct {
	router *modelRouter
}

func (c *copilotClient) Name() string { return "github-copilot" }

// SetModelMetadata lets runtime-owned model switches reach the wire clients.
func (c *copilotClient) SetModelMetadata(model Model) { c.router.SetModelMetadata(model) }

func (c *copilotClient) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	metadata := copilotRequestMetadata{initiator: "user"}
	if len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role != RoleUser {
		metadata.initiator = "agent"
	}
	for _, message := range req.Messages {
		if message.Role == RoleUser || message.Role == RoleTool {
			metadata.vision = metadata.vision || copilotHasImages(message.Content)
		}
	}
	// One catalog lookup decides both reasoning and route.
	model, findErr := FindModel(c.Name(), req.Model)
	tagged := findErr == nil && model.API != ""
	api := copilotModelAPI(req.Model)
	if tagged {
		api = model.API
	}
	// Chat Completions endpoints perform their own thinking; they reject
	// reasoning_effort.
	if api == APICompletions {
		req.Reasoning = ""
	}
	ctx = context.WithValue(ctx, copilotRequestKey{}, metadata)
	if tagged {
		return c.router.Stream(ctx, req)
	}
	// Use family routing for custom models that have not supplied an API tag.
	routed := c.router.byAPI[api]
	if routed == nil {
		return nil, fmt.Errorf("provider %q model %q has no client for wire API %q", c.Name(), req.Model, api)
	}
	return routed.Stream(ctx, req)
}

func copilotHasImages(blocks []Content) bool {
	for _, block := range blocks {
		switch b := block.(type) {
		case ImageBlock:
			return true
		case ToolResultBlock:
			if copilotHasImages(b.Content) {
				return true
			}
		}
	}
	return false
}

func copilotModelAPI(id string) string {
	switch {
	case strings.HasPrefix(id, "claude-"):
		return APIAnthropicMessages
	case strings.HasPrefix(id, "gpt-4"), strings.HasPrefix(id, "grok-code-"):
		// Legacy chat-only families keep Chat Completions.
		return APICompletions
	case strings.HasPrefix(id, "gpt-"), strings.HasPrefix(id, "grok-"), strings.HasPrefix(id, "mai-"), strings.HasPrefix(id, "oswe"):
		return APIResponses
	default:
		return APICompletions
	}
}

func configureCopilotModel(m *Model) {
	switch m.API {
	case "":
		m.API = copilotModelAPI(m.ID)
	case "anthropic":
		// Legacy models.json api values map onto Copilot's wire protocols.
		m.API = APIAnthropicMessages
	case "openai":
		m.API = APICompletions
	}
	// Claude Haiku, 4.5 and 4.6 use explicit budgets: the canonical Anthropic
	// metadata (usesAdaptiveThinking) does not treat 4.6 as adaptive. Newer or
	// unknown Claude ids keep the adaptive default; explicit metadata wins.
	if m.API == APIAnthropicMessages && !usesExplicitThinkingBudget(m.ID) {
		m.AdaptiveThinking = true
	}
	// An explicit mapping (for example from models.json) always wins.
	if m.ReasoningLevelMap != nil {
		return
	}
	if m.API == APICompletions {
		m.ReasoningLevelMap = map[string]string{"minimum": "", "low": "", "medium": "", "high": "", "xhigh": "", "max": ""}
	}
	// Responses defaults expose xhigh, but these models only support low/medium/high.
	if m.ID == "gpt-5-mini" || m.ID == "grok-4.5" || strings.HasPrefix(m.ID, "mai-") {
		m.ReasoningLevelMap = map[string]string{"xhigh": "", "max": ""}
	}
}

func usesExplicitThinkingBudget(id string) bool {
	if strings.HasPrefix(id, "claude-haiku-") {
		return true
	}
	// Claude 4 through 4.6 predate adaptive thinking (4.7 is the first).
	return copilotLegacyClaudeVersion.MatchString(id)
}

var copilotLegacyClaudeVersion = regexp.MustCompile(`-4(\.[0-6])?$`)
