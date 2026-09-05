package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func (r Resolved) newInteractiveAgent() *core.Agent {
	ag := r.NewAgent()
	modes.BindCodexWeeklyUsageFetcher(ag, r.codexWeeklyUsageFetcher())
	return ag
}

// Build the optional meter from the resolved route, not the TUI's startup auth
// metadata. Rebuilds replace it alongside the model client (including rescue).
func (r Resolved) codexWeeklyUsageFetcher() func(context.Context) (*provider.CodexWeeklyUsage, error) {
	if r.Provider != "openai-codex" || r.AuthMethod != "oauth" || r.AccountID == "" ||
		(r.BaseURL != "" && strings.TrimRight(r.BaseURL, "/") != "https://chatgpt.com/backend-api/codex/responses") {
		return nil
	}
	accountID := r.AccountID
	return func(ctx context.Context) (*provider.CodexWeeklyUsage, error) {
		tok := loadOAuthToken("openai")
		if tok == nil || tok.AccountID != accountID {
			return nil, fmt.Errorf("codex usage: subscription account changed")
		}
		// A passive meter must not rotate credentials alongside an inference
		// request or write them back after logout. Normal model requests/login
		// own refresh; the next poll picks up their newly stored token.
		if tok.Expired() {
			return nil, fmt.Errorf("codex usage: subscription token expired")
		}
		return provider.FetchCodexWeeklyUsage(ctx, nil, tok.AccessToken, accountID)
	}
}
