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
	openaiCodexModelsBaseURL = "https://chatgpt.com/backend-api/codex"
	openaiCodexClientVersion = "0.144.0"
	openrouterDefaultBaseURL = "https://openrouter.ai/api/v1"
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
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai-codex discover http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var page struct {
		Models []struct {
			Slug           string `json:"slug"`
			DisplayName    string `json:"display_name"`
			SupportedInAPI bool   `json:"supported_in_api"`
			ContextWindow  *int   `json:"context_window"`
			MaxContext     *int   `json:"max_context_window"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("openai-codex discover parse: %w", err)
	}
	out := make([]Model, 0, len(page.Models))
	for _, m := range page.Models {
		if m.Slug == "" || !m.SupportedInAPI {
			continue
		}
		contextWindow := 0
		if m.ContextWindow != nil {
			contextWindow = *m.ContextWindow
		}
		if contextWindow == 0 && m.MaxContext != nil {
			contextWindow = *m.MaxContext
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
			Reasoning:     true,
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
