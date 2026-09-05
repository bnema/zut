package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// CodexWeeklyUsage is the account-wide seven-day allowance, not session usage.
// A nil result means the backend did not report a weekly window.
type CodexWeeklyUsage struct {
	RemainingPercent float64
	ResetsAt         time.Time
}

// FetchCodexWeeklyUsage reads ChatGPT subscription usage without making a model
// request. Credentials must belong to the selected ChatGPT account. The caller
// owns credential refresh and must provide a bounded context.
func FetchCodexWeeklyUsage(ctx context.Context, client *http.Client, token, accountID string) (*CodexWeeklyUsage, error) {
	return fetchCodexWeeklyUsage(ctx, client, codexUsageURL, token, accountID)
}

func fetchCodexWeeklyUsage(ctx context.Context, client *http.Client, endpoint, token, accountID string) (*CodexWeeklyUsage, error) {
	if token == "" || accountID == "" {
		return nil, fmt.Errorf("codex usage: missing subscription credentials")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("codex usage: invalid endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("ChatGPT-Account-Id", accountID)
	req.Header.Set("User-Agent", "codex-cli")
	req.Header.Set("Accept", "application/json")
	if client == nil {
		client = http.DefaultClient
	}
	// The account header is sensitive too: never forward it on a redirect.
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := bounded.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex usage: request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex usage: HTTP %d", resp.StatusCode)
	}
	type window struct {
		UsedPercent *float64 `json:"used_percent"`
		Seconds     int      `json:"limit_window_seconds"`
		ResetAt     int64    `json:"reset_at"`
	}
	var payload struct {
		RateLimit *struct {
			Primary   *window `json:"primary_window"`
			Secondary *window `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, fmt.Errorf("codex usage: unreadable or oversized response")
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		// Do not include response text or JSON errors containing account data.
		return nil, fmt.Errorf("codex usage: invalid response")
	}
	if payload.RateLimit == nil {
		return nil, nil
	}
	for _, w := range []*window{payload.RateLimit.Primary, payload.RateLimit.Secondary} {
		if w == nil || w.Seconds != 7*24*60*60 {
			continue
		}
		if w.UsedPercent == nil || *w.UsedPercent < 0 || *w.UsedPercent > 100 {
			return nil, fmt.Errorf("codex usage: invalid weekly percentage")
		}
		usage := &CodexWeeklyUsage{RemainingPercent: 100 - *w.UsedPercent}
		if w.ResetAt > 0 {
			usage.ResetsAt = time.Unix(w.ResetAt, 0)
		}
		return usage, nil
	}
	return nil, nil
}
