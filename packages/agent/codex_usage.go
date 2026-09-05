package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/provider"
)

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
		tok, err := refreshIfExpiredContext(ctx, "openai", tok)
		if err != nil {
			return nil, err
		}
		return provider.FetchCodexWeeklyUsage(ctx, nil, tok.AccessToken, accountID)
	}
}
