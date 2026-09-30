package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// DiscoverCopilotAvailableModels lists the account's selectable tool-capable
// models. It does not enable model policies or persist account-specific data.
func DiscoverCopilotAvailableModels(ctx context.Context, pat string) ([]string, error) {
	token, err := copilotCache.get(ctx, pat)
	if err != nil {
		return nil, err
	}
	baseURL := token.baseURL
	if baseURL == "" {
		baseURL = copilotDefaultBaseURL
	}
	client := &http.Client{Transport: &copilotRefreshTransport{inner: http.DefaultTransport, pat: pat}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2026-06-01")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("copilot model discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot model discovery: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("copilot model discovery: %w", err)
	}
	// The fallback is deliberately restricted to the individual endpoint.
	individual := baseURL == copilotDefaultBaseURL
	return parseCopilotAvailableModels(body, individual)
}

func parseCopilotAvailableModels(body []byte, allowPolicyFallback bool) ([]string, error) {
	var catalog struct {
		Data []struct {
			ID     string `json:"id"`
			Picker bool   `json:"model_picker_enabled"`
			Policy struct {
				State string `json:"state"`
			} `json:"policy"`
			Capabilities struct {
				Supports struct {
					ToolCalls *bool `json:"tool_calls"`
				} `json:"supports"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil || catalog.Data == nil {
		return nil, fmt.Errorf("copilot model discovery: invalid models response")
	}
	picker := make([]string, 0)
	enabled := make([]string, 0)
	seen := make(map[string]bool)
	for _, m := range catalog.Data {
		if m.ID == "" || seen[m.ID] || (m.Capabilities.Supports.ToolCalls != nil && !*m.Capabilities.Supports.ToolCalls) {
			continue
		}
		seen[m.ID] = true
		if m.Picker && m.Policy.State != "disabled" {
			picker = append(picker, m.ID)
		}
		if m.Policy.State == "enabled" {
			enabled = append(enabled, m.ID)
		}
	}
	if len(picker) == 0 && allowPolicyFallback {
		return enabled, nil
	}
	return picker, nil
}
