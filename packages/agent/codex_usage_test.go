package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider/auth"
)

func TestCodexWeeklyUsageFetcherEligibility(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resolved Resolved
		want     bool
	}{
		{"subscription", Resolved{Provider: "openai-codex", AuthMethod: "oauth", AccountID: "synthetic"}, true},
		{"canonical endpoint", Resolved{Provider: "openai-codex", AuthMethod: "oauth", AccountID: "synthetic", BaseURL: "https://chatgpt.com/backend-api/codex/responses/"}, true},
		{"api key", Resolved{Provider: "openai-codex", AuthMethod: "apikey", AccountID: "synthetic"}, false},
		{"no account", Resolved{Provider: "openai-codex", AuthMethod: "oauth"}, false},
		{"other provider", Resolved{Provider: "openai", AuthMethod: "oauth", AccountID: "synthetic"}, false},
		{"custom endpoint", Resolved{Provider: "openai-codex", AuthMethod: "oauth", AccountID: "synthetic", BaseURL: "https://custom.example/responses"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.resolved.codexWeeklyUsageFetcher() != nil; got != tc.want {
				t.Fatalf("fetcher available = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestCodexWeeklyUsageFetcherRejectsMissingChangedAndExpiredCredentials(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	fetch := (Resolved{Provider: "openai-codex", AuthMethod: "oauth", AccountID: "synthetic-account"}).codexWeeklyUsageFetcher()
	if _, err := fetch(context.Background()); err == nil {
		t.Fatal("missing credential accepted")
	}
	store := AuthStoreFor()
	if err := store.SetOAuth("openai", auth.OAuthToken{AccessToken: "synthetic-token", AccountID: "different-account", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := fetch(context.Background()); err == nil {
		t.Fatal("different account accepted")
	}
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	original := auth.OpenAIOAuth.TokenURL
	auth.OpenAIOAuth.TokenURL = srv.URL
	t.Cleanup(func() { auth.OpenAIOAuth.TokenURL = original })
	if err := store.SetOAuth("openai", auth.OAuthToken{AccessToken: "synthetic-token", RefreshToken: "synthetic-refresh", AccountID: "synthetic-account", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := fetch(context.Background()); err == nil {
		t.Fatal("expired credential accepted")
	}
	if refreshCalls.Load() != 0 {
		t.Fatal("passive usage meter attempted to rotate credentials")
	}
}
