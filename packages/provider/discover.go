package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DiscoverAnthropic lists model ids visible to key on api.anthropic.com.
// The API returns a paginated list; we page through until has_more is false.
func DiscoverAnthropic(ctx context.Context, apiKey, baseURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = anthropicDefaultBaseURL
	}
	client := &http.Client{Timeout: 15 * time.Second}
	var out []Model
	after := ""
	for {
		url := strings.TrimRight(baseURL, "/") + "/v1/models?limit=1000"
		if after != "" {
			url += "&after_id=" + after
		}
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", anthropicAPIVersion)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("anthropic discover http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var page struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("anthropic discover parse: %w", err)
		}
		for _, d := range page.Data {
			out = append(out, Model{
				Provider:    "anthropic",
				ID:          d.ID,
				DisplayName: d.DisplayName,
				Source:      "live",
			})
		}
		if !page.HasMore || page.LastID == "" {
			break
		}
		after = page.LastID
	}
	return out, nil
}

// DiscoverOpenAI lists model ids visible to key on api.openai.com.
func DiscoverOpenAI(ctx context.Context, apiKey, baseURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = openaiDefaultBaseURL
	}
	client := &http.Client{Timeout: 15 * time.Second}
	url := strings.TrimRight(baseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openai discover http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var page struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("openai discover parse: %w", err)
	}
	var out []Model
	for _, d := range page.Data {
		// Keep only chat-capable families. OpenAI's /v1/models returns
		// everything including embeddings, TTS, DALL-E, etc.
		if !looksLikeChatModel(d.ID) {
			continue
		}
		out = append(out, Model{
			Provider:    "openai",
			ID:          d.ID,
			DisplayName: d.ID,
			Source:      "live",
		})
	}
	return out, nil
}

const (
	modelsDevAPIURL          = "https://models.dev/api.json"
	openCodeGoDefaultBaseURL = "https://opencode.ai/zen/go/v1"
)

// DiscoverOpenCodeGo joins the account's currently served model ids with
// model metadata from models.dev. The OpenCode Go /models endpoint is the
// authority for availability; models.dev supplies display names, limits,
// reasoning capabilities, and pricing.
func DiscoverOpenCodeGo(ctx context.Context, apiKey, baseURL string) ([]Model, error) {
	return discoverOpenCodeGo(ctx, apiKey, baseURL, modelsDevAPIURL)
}

// discoverOpenCodeGo is split out so tests can use a local models.dev fixture.
func discoverOpenCodeGo(ctx context.Context, apiKey, baseURL, metadataURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = openCodeGoDefaultBaseURL
	}
	if metadataURL == "" {
		metadataURL = modelsDevAPIURL
	}

	client := &http.Client{Timeout: 15 * time.Second}
	metadataBody, err := fetchDiscoveryJSON(ctx, client, metadataURL, "")
	if err != nil {
		return nil, fmt.Errorf("opencode-go metadata: %w", err)
	}
	var providers map[string]modelsDevProvider
	if err := json.Unmarshal(metadataBody, &providers); err != nil {
		return nil, fmt.Errorf("opencode-go metadata parse: %w", err)
	}
	metadata, ok := providers["opencode-go"]
	if !ok {
		return nil, fmt.Errorf("opencode-go metadata provider is missing")
	}
	modelsBody, err := fetchDiscoveryJSON(ctx, client, strings.TrimRight(baseURL, "/")+"/models", "Bearer "+apiKey)
	if err != nil {
		return nil, fmt.Errorf("opencode-go models: %w", err)
	}
	var page struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(modelsBody, &page); err != nil {
		return nil, fmt.Errorf("opencode-go models parse: %w", err)
	}

	out := make([]Model, 0, len(page.Data))
	for _, live := range page.Data {
		if live.ID == "" {
			continue
		}
		model := Model{
			Provider:    "opencode-go",
			ID:          live.ID,
			DisplayName: live.ID,
			BaseURL:     strings.TrimRight(baseURL, "/"),
			Source:      "live",
			API:         openCodeGoAPIForModel(live.ID),
		}
		if details, ok := metadata.Models[live.ID]; ok {
			if details.Name != "" {
				model.DisplayName = details.Name
			}
			model.ContextWindow = details.Limit.Context
			model.MaxOutput = details.Limit.Output
			model.Reasoning = details.Reasoning
			model.ReasoningLevelMap, model.ReasoningEffortMap = modelsDevReasoningMaps(details.Reasoning, details.ReasoningOptions)
			model.PriceInput = details.Cost.Input
			model.PriceOutput = details.Cost.Output
			model.PriceCacheRead = details.Cost.CacheRead
			model.PriceCacheWrite = details.Cost.CacheWrite
			for _, tier := range details.Cost.Tiers {
				if tier.Tier.Type != "context" || tier.Tier.Size <= 0 {
					continue
				}
				model.PriceTierInputTokens = tier.Tier.Size
				model.PriceInputAbove = tier.Input
				model.PriceOutputAbove = tier.Output
				model.PriceCacheReadAbove = tier.CacheRead
				model.PriceCacheWriteAbove = tier.CacheWrite
				break
			}
		}
		out = append(out, model)
	}
	return out, nil
}

// modelsDevProvider and its nested types intentionally cover only the stable
// subset needed by provider.Model. models.dev adds fields over time, and the
// JSON decoder safely ignores those additions.
type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	Name             string                     `json:"name"`
	Reasoning        bool                       `json:"reasoning"`
	ReasoningOptions []modelsDevReasoningOption `json:"reasoning_options"`
	Limit            modelsDevLimit             `json:"limit"`
	Cost             modelsDevCost              `json:"cost"`
}

type modelsDevReasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}

type modelsDevLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type modelsDevCost struct {
	Input      float64             `json:"input"`
	Output     float64             `json:"output"`
	CacheRead  float64             `json:"cache_read"`
	CacheWrite float64             `json:"cache_write"`
	Tiers      []modelsDevCostTier `json:"tiers"`
}

type modelsDevCostTier struct {
	Input      float64            `json:"input"`
	Output     float64            `json:"output"`
	CacheRead  float64            `json:"cache_read"`
	CacheWrite float64            `json:"cache_write"`
	Tier       modelsDevCostLimit `json:"tier"`
}

type modelsDevCostLimit struct {
	Type string `json:"type"`
	Size int    `json:"size"`
}

// modelsDevReasoningMaps translates the effort values used by models.dev
// into zut's reasoning levels and exact provider wire values. Generic OpenAI
// defaults include low/medium/high; remove unsupported defaults and add
// supported minimum/xhigh/max levels.
func modelsDevReasoningMaps(reasoning bool, options []modelsDevReasoningOption) (map[string]string, map[string]string) {
	if !reasoning {
		return nil, nil
	}
	supported := make(map[string]string)
	for _, option := range options {
		if option.Type != "effort" {
			continue
		}
		for _, value := range option.Values {
			level := NormalizeReasoning(value)
			if reasoningLevelRank(level) > 0 {
				supported[level] = strings.ToLower(strings.TrimSpace(value))
			}
		}
	}
	if len(supported) == 0 {
		return nil, nil
	}

	levels := []string{"minimum", "low", "medium", "high", "xhigh", "max"}
	defaults := map[string]bool{"low": true, "medium": true, "high": true}
	levelMap := make(map[string]string)
	effortMap := make(map[string]string)
	for _, level := range levels {
		switch {
		case supported[level] != "":
			levelMap[level] = level
			effortMap[level] = supported[level]
		case defaults[level]:
			levelMap[level] = ""
		}
	}
	return levelMap, effortMap
}

// OpenCode Go currently serves GPT-5.6 models on its Responses endpoint while
// the rest of the Go catalog uses Chat Completions. Keep this rule based on the
// model family rather than maintaining another list of model ids.
func openCodeGoAPIForModel(id string) string {
	if strings.HasPrefix(strings.ToLower(id), "gpt-5.6-") {
		return APIResponses
	}
	return ""
}

func fetchDiscoveryJSON(ctx context.Context, client *http.Client, url, authorization string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if authorization != "" {
		req.Header.Set("authorization", authorization)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxDiscoveryResponseBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxDiscoveryResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// DiscoverGoogle lists Gemini model ids visible to key on
// generativelanguage.googleapis.com. The API paginates with
// nextPageToken; we follow it until exhausted.
func DiscoverGoogle(ctx context.Context, apiKey, baseURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = geminiDefaultBaseURL
	}
	client := &http.Client{Timeout: 15 * time.Second}
	var out []Model
	pageToken := ""
	for {
		url := strings.TrimRight(baseURL, "/") + "/v1beta/models?pageSize=200"
		if pageToken != "" {
			url += "&pageToken=" + pageToken
		}
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-goog-api-key", apiKey)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("google discover http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var page struct {
			Models []struct {
				Name                       string   `json:"name"` // "models/gemini-2.5-pro"
				DisplayName                string   `json:"displayName"`
				SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
				InputTokenLimit            int      `json:"inputTokenLimit"`
				OutputTokenLimit           int      `json:"outputTokenLimit"`
			} `json:"models"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("google discover parse: %w", err)
		}
		for _, m := range page.Models {
			// Strip the "models/" prefix Gemini uses on resource names.
			id := strings.TrimPrefix(m.Name, "models/")
			if !looksLikeGeminiChatModel(id, m.SupportedGenerationMethods) {
				continue
			}
			display := m.DisplayName
			if display == "" {
				display = id
			}
			out = append(out, Model{
				Provider:      "google",
				ID:            id,
				DisplayName:   display,
				ContextWindow: m.InputTokenLimit,
				MaxOutput:     m.OutputTokenLimit,
				Source:        "live",
			})
		}
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	return out, nil
}

// looksLikeGeminiChatModel filters Gemini's /v1beta/models output
// down to entries usable with streamGenerateContent. The API also
// lists embedding models, AQA, and other non-chat artefacts.
func looksLikeGeminiChatModel(id string, methods []string) bool {
	if !strings.HasPrefix(id, "gemini-") && !strings.HasPrefix(id, "gemma-") {
		return false
	}
	if strings.Contains(id, "embedding") || strings.Contains(id, "aqa") {
		return false
	}
	if len(methods) > 0 {
		ok := false
		for _, m := range methods {
			if m == "generateContent" || m == "streamGenerateContent" {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// looksLikeChatModel returns true for OpenAI ids that can plausibly be
// used with the chat/completions endpoint. Errs on the side of inclusion.
func looksLikeChatModel(id string) bool {
	switch {
	case strings.HasPrefix(id, "gpt-"):
		return true
	case strings.HasPrefix(id, "o1"):
		return true
	case strings.HasPrefix(id, "o3"):
		return true
	case strings.HasPrefix(id, "o4"):
		return true
	case strings.HasPrefix(id, "o5"):
		return true
	case strings.HasPrefix(id, "chatgpt-"):
		return true
	}
	return false
}

const (
	openaiCodexModelsBaseURL  = "https://chatgpt.com/backend-api/codex"
	openaiCodexClientVersion  = "0.144.0"
	openrouterDefaultBaseURL  = "https://openrouter.ai/api/v1"
	maxDiscoveryResponseBytes = 10 << 20
)

// DiscoverOpenAICodex lists the models available to a ChatGPT/Codex OAuth
// account. This catalog is separate from api.openai.com/v1/models and is the
// authoritative source for models usable through the Codex subscription route.
func DiscoverOpenAICodex(ctx context.Context, token, accountID, baseURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = openaiCodexModelsBaseURL
	}
	url := strings.TrimRight(baseURL, "/") + "/models?client_version=" + openaiCodexClientVersion
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+token)
	if accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}
	// The Codex backend expects the same stable client identity used for
	// requests to its Responses endpoint.
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("user-agent", "codex_cli_rs/0.0.0")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("openai-codex discover read response: %w", err)
	}
	if len(body) > maxDiscoveryResponseBytes {
		return nil, fmt.Errorf("openai-codex discover response exceeds %d bytes", maxDiscoveryResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai-codex discover http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var page struct {
		Models []struct {
			Slug                     string   `json:"slug"`
			DisplayName              string   `json:"display_name"`
			SupportedInAPI           bool     `json:"supported_in_api"`
			Visibility               string   `json:"visibility"`
			SupportedReasoningLevels []string `json:"supported_reasoning_levels"`
			ContextWindow            *int     `json:"context_window"`
			MaxContext               *int     `json:"max_context_window"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("openai-codex discover parse: %w", err)
	}
	out := make([]Model, 0, len(page.Models))
	for _, m := range page.Models {
		if m.Slug == "" || !m.SupportedInAPI || m.Visibility == "hide" {
			continue
		}
		contextWindow := 0
		if m.ContextWindow != nil {
			contextWindow = *m.ContextWindow
		}
		if m.MaxContext != nil {
			switch {
			case contextWindow == 0:
				contextWindow = *m.MaxContext
			case *m.MaxContext >= openAIContextWindowTarget && contextWindow < openAIContextWindowTarget:
				contextWindow = openAIContextWindowTarget
			}
		}
		displayName := m.DisplayName
		if displayName == "" {
			displayName = m.Slug
		}
		out = append(out, Model{
			Provider:      "openai-codex",
			ID:            m.Slug,
			DisplayName:   displayName,
			ContextWindow: contextWindow,
			Reasoning:     len(m.SupportedReasoningLevels) > 0,
			Source:        "live",
		})
	}
	return out, nil
}

// DiscoverOpenRouter lists models from OpenRouter's public /models
// endpoint (no auth). Per-token USD prices are converted to USD per 1M
// tokens to match the rest of the catalog. baseURL defaults to the
// public endpoint.
func DiscoverOpenRouter(ctx context.Context, baseURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = openrouterDefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: 15 * time.Second}
	url := baseURL + "/models"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openrouter discover http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var page struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int    `json:"context_length"`
			Pricing       struct {
				Prompt          string `json:"prompt"`
				Completion      string `json:"completion"`
				InputCacheRead  string `json:"input_cache_read"`
				InputCacheWrite string `json:"input_cache_write"`
			} `json:"pricing"`
			TopProvider struct {
				ContextLength       int  `json:"context_length"`
				MaxCompletionTokens *int `json:"max_completion_tokens"`
			} `json:"top_provider"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("openrouter discover parse: %w", err)
	}
	var out []Model
	for _, d := range page.Data {
		if d.ID == "" {
			continue
		}
		display := d.Name
		if display == "" {
			display = d.ID
		}
		ctxWin := d.ContextLength
		if d.TopProvider.ContextLength > 0 && (ctxWin == 0 || d.TopProvider.ContextLength < ctxWin) {
			ctxWin = d.TopProvider.ContextLength
		}
		maxOut := 0
		if d.TopProvider.MaxCompletionTokens != nil {
			maxOut = *d.TopProvider.MaxCompletionTokens
		}
		out = append(out, Model{
			Provider:        "openrouter",
			ID:              d.ID,
			DisplayName:     display,
			ContextWindow:   ctxWin,
			MaxOutput:       maxOut,
			Reasoning:       openrouterSupportsReasoning(d.SupportedParameters),
			PriceInput:      perMillionTokens(d.Pricing.Prompt),
			PriceOutput:     perMillionTokens(d.Pricing.Completion),
			PriceCacheRead:  perMillionTokens(d.Pricing.InputCacheRead),
			PriceCacheWrite: perMillionTokens(d.Pricing.InputCacheWrite),
			BaseURL:         baseURL,
			Source:          "live",
		})
	}
	return out, nil
}

// openrouterSupportsReasoning reports whether OpenRouter's
// supported_parameters list marks the model as reasoning-capable.
func openrouterSupportsReasoning(params []string) bool {
	for _, p := range params {
		if p == "reasoning" || p == "include_reasoning" {
			return true
		}
	}
	return false
}

// perMillionTokens converts a per-token USD price string to USD per 1M
// tokens. Empty or unparseable values become 0.
func perMillionTokens(s string) float64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v * 1_000_000
}
