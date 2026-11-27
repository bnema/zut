// Package agent wires the provider, core, tools, auth, and modes into a CLI.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/config"
	providerpkg "github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/auth"
)

// AuthStoreFor returns the auth.Store backed by config.AuthPath().
func AuthStoreFor() *auth.Store { return auth.NewStore(config.AuthPath()) }

// ResolveLlamaCPPConfig resolves the router URL and optional API key. The
// environment overrides the credential stored through /login.
func ResolveLlamaCPPConfig() (baseURL, apiKey string, err error) {
	return resolveLlamaCPPConfig(context.Background(), apiKeyCommandExecute)
}

func resolveLlamaCPPConfig(ctx context.Context, commandMode apiKeyCommandMode) (baseURL, apiKey string, err error) {
	return resolveLlamaCPPConfigEnv(ctx, commandMode, os.Getenv)
}

func resolveLlamaCPPConfigEnv(ctx context.Context, commandMode apiKeyCommandMode, getenv func(string) string) (baseURL, apiKey string, err error) {
	baseURL = strings.TrimSpace(getenv("LLAMA_BASE_URL"))
	apiKey = strings.TrimSpace(getenv("LLAMA_API_KEY"))
	if baseURL == "" {
		creds, loadErr := AuthStoreFor().Load()
		if loadErr != nil {
			return "", "", loadErr
		}
		if stored, ok := creds.AdditionalAPIKeyCreds[providerpkg.LlamaCPPProviderID]; ok {
			baseURL = stored.BaseURL
			if apiKey == "" {
				if stored.APIKeyCommand != nil && commandMode == apiKeyCommandSkip {
					return "", "", nil
				}
				if key, found, commandErr := resolveStoredAPIKey(ctx, providerpkg.LlamaCPPProviderID, stored, commandMode); found || commandErr != nil {
					if commandErr != nil {
						return "", "", commandErr
					}
					apiKey = key
				}
			}
		}
	}
	if baseURL == "" {
		return "", apiKey, nil
	}
	baseURL, err = providerpkg.NormalizeLlamaCPPURL(baseURL)
	return baseURL, apiKey, err
}

// ResolveCredential returns the credential (api key or oauth access
// token), the method ("apikey"/"oauth"), and an error when no
// credential is available.
//
// Lookup order:
//  1. explicit (e.g. --api-key): treated as API key
//  2. provider-specific env var: treated as API key
//  3. auth.json: api key OR oauth, whichever is present
func ResolveCredential(provider, explicit string) (cred, method string, err error) {
	cred, method, _, err = ResolveCredentialFull(provider, explicit)
	return cred, method, err
}

// ResolveCredentialContext is ResolveCredential with caller cancellation.
func ResolveCredentialContext(ctx context.Context, provider, explicit string) (cred, method string, err error) {
	cred, method, _, err = ResolveCredentialFullContext(ctx, provider, explicit)
	return cred, method, err
}

// ResolveCredentialFull is like ResolveCredential but also returns a
// provider-specific accountID when the credential is an OpenAI OAuth
// token (the ChatGPT account id extracted from the stored id_token).
// accountID is "" for API-key auth and for anthropic.
func ResolveCredentialFull(provider, explicit string) (cred, method, accountID string, err error) {
	return ResolveCredentialFullContext(context.Background(), provider, explicit)
}

// ResolveCredentialFullContext is ResolveCredentialFull with caller cancellation.
func ResolveCredentialFullContext(ctx context.Context, provider, explicit string) (cred, method, accountID string, err error) {
	return resolveCredentialFull(ctx, provider, explicit, apiKeyCommandExecute)
}

type apiKeyCommandMode uint8

const (
	apiKeyCommandExecute apiKeyCommandMode = iota
	apiKeyCommandAvailable
	apiKeyCommandSkip
)

func resolveCredentialFull(ctx context.Context, provider, explicit string, commandMode apiKeyCommandMode) (cred, method, accountID string, err error) {
	return resolveCredentialFullEnv(ctx, provider, explicit, commandMode, os.Getenv)
}

// resolveCredentialFullEnv reads provider env vars through getenv so a
// daemon-owned session can resolve from its client's environment.
func resolveCredentialFullEnv(ctx context.Context, provider, explicit string, commandMode apiKeyCommandMode, getenv func(string) string) (cred, method, accountID string, err error) {
	if explicit != "" {
		return explicit, "apikey", "", nil
	}
	switch provider {
	case "anthropic":
		// ANTHROPIC_OAUTH_TOKEN takes precedence over ANTHROPIC_API_KEY.
		// Useful when both are set and the user wants subscription auth
		// without editing auth.json.
		if v := getenv("ANTHROPIC_OAUTH_TOKEN"); v != "" {
			return v, "oauth", "", nil
		}
		if v := getenv("ANTHROPIC_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "openai":
		if v := getenv("OPENAI_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "openai-codex":
		// ChatGPT/Codex subscription route. It intentionally ignores
		// OPENAI_API_KEY so users can keep both OpenAI API and Codex
		// subscription credentials configured and choose by provider.
	case "openai-responses":
		// Public OpenAI Responses API. Same env var as the chat-completions
		// `openai` provider; users pick the wire format by provider id.
		if v := getenv("OPENAI_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "kimi":
		if v := getenv("KIMI_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
		if v := getenv("MOONSHOT_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "google":
		// Both env names are widely-used in the Google ecosystem;
		// GEMINI_API_KEY is the AI Studio default, GOOGLE_API_KEY
		// is the older / generic name. Either works.
		if v := getenv("GEMINI_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
		if v := getenv("GOOGLE_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "deepseek":
		if v := getenv("DEEPSEEK_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	// ollama needs no explicit case: the generic
	// normalizeCustomProviderEnvVar fallback below already maps
	// provider "ollama" to OLLAMA_API_KEY before auth.json.
	case "llama.cpp":
		baseURL, apiKey, resolveErr := resolveLlamaCPPConfig(ctx, commandMode)
		if resolveErr != nil {
			return "", "", "", resolveErr
		}
		if baseURL != "" {
			return firstNonEmpty(apiKey, "local"), "apikey", "", nil
		}
	case "moonshotai", "moonshotai-cn":
		// Moonshot direct API (separate from kimi-coding, which is the
		// Anthropic-Messages-fronted /coding endpoint with subscription OAuth).
		if v := getenv("MOONSHOT_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "groq":
		if v := getenv("GROQ_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "xai":
		if v := getenv("XAI_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "cerebras":
		if v := getenv("CEREBRAS_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "together":
		if v := getenv("TOGETHER_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "huggingface":
		if v := getenv("HF_TOKEN"); v != "" {
			return v, "apikey", "", nil
		}
	case "openrouter":
		if v := getenv("OPENROUTER_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "mistral":
		if v := getenv("MISTRAL_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "zai":
		if v := getenv("ZAI_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "xiaomi", "xiaomi-token-plan-ams", "xiaomi-token-plan-cn", "xiaomi-token-plan-sgp":
		envVar := "XIAOMI_API_KEY"
		switch provider {
		case "xiaomi-token-plan-ams":
			envVar = "XIAOMI_TOKEN_PLAN_AMS_API_KEY"
		case "xiaomi-token-plan-cn":
			envVar = "XIAOMI_TOKEN_PLAN_CN_API_KEY"
		case "xiaomi-token-plan-sgp":
			envVar = "XIAOMI_TOKEN_PLAN_SGP_API_KEY"
		}
		if v := getenv(envVar); v != "" {
			return v, "apikey", "", nil
		}
	case "minimax":
		if v := getenv("MINIMAX_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "minimax-cn":
		if v := getenv("MINIMAX_CN_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
		if v := getenv("MINIMAX_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "fireworks":
		if v := getenv("FIREWORKS_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "vercel-ai-gateway":
		if v := getenv("AI_GATEWAY_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "opencode", "opencode-go":
		if v := getenv("OPENCODE_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "github-copilot":
		if v := getenv("COPILOT_GITHUB_TOKEN"); v != "" {
			return v, "apikey", "", nil
		}
		if v := getenv("GITHUB_COPILOT_TOKEN"); v != "" {
			return v, "apikey", "", nil
		}
	case "cloudflare-workers-ai", "cloudflare-ai-gateway":
		if v := getenv("CLOUDFLARE_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "amazon-bedrock":
		// Bedrock has many credential sources (AWS_PROFILE, IAM keys,
		// container creds, IRSA, bearer token). We surface a sentinel so
		// Resolve doesn't error on missing key; the real client (when
		// implemented) will resolve credentials through aws-sdk-go-v2.
		if getenv("AWS_ACCESS_KEY_ID") != "" || getenv("AWS_PROFILE") != "" ||
			getenv("AWS_BEARER_TOKEN_BEDROCK") != "" {
			return "<aws>", "apikey", "", nil
		}
	case "google-vertex":
		if v := getenv("GOOGLE_CLOUD_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
		// Check for Application Default Credentials (gcloud auth application-default login)
		// or service account JSON, matching the actual NewVertex client behavior.
		if getenv("GOOGLE_APPLICATION_CREDENTIALS") != "" {
			return "<adc>", "apikey", "", nil
		}
		// Check for the platform-specific default ADC path that gcloud writes to.
		if adcPath, err := auth.GoogleApplicationDefaultCredentialsPath(); err == nil {
			if _, err := os.Stat(adcPath); err == nil {
				return "<adc>", "apikey", "", nil
			}
		}
	case "azure-openai-responses":
		if v := getenv("AZURE_OPENAI_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	}
	// Generic env var fallback for custom providers: normalize the
	// provider id to a shell-friendly env var name (hyphens to
	// underscores) and check {NAME}_API_KEY before auth.json.
	if v := getenv(normalizeCustomProviderEnvVar(provider) + "_API_KEY"); v != "" {
		return v, "apikey", "", nil
	}
	c, err := AuthStoreFor().Load()
	if err != nil {
		return "", "", "", err
	}
	if pc, ok := storedProviderCreds(c, provider); ok {
		if key, found, commandErr := resolveStoredAPIKey(ctx, provider, pc, commandMode); found || commandErr != nil {
			if commandErr != nil {
				return "", "", "", commandErr
			}
			return key, "apikey", "", nil
		}
		if provider == "xai" && pc.OAuth != nil && pc.OAuth.AccessToken != "" {
			tok, _ := refreshIfExpiredContext(ctx, provider, pc.OAuth)
			return tok.AccessToken, "oauth", "", nil
		}
	}
	switch provider {
	case "anthropic":
		if c.Anthropic.OAuth != nil && c.Anthropic.OAuth.AccessToken != "" {
			tok, _ := refreshIfExpiredContext(ctx, "anthropic", c.Anthropic.OAuth)
			return tok.AccessToken, "oauth", "", nil
		}
	case "openai-codex":
		if c.OpenAI.OAuth != nil && c.OpenAI.OAuth.AccessToken != "" {
			tok, _ := refreshIfExpiredContext(ctx, "openai", c.OpenAI.OAuth)
			return tok.AccessToken, "oauth", tok.AccountID, nil
		}
	case "kimi":
		if c.Kimi.OAuth != nil && c.Kimi.OAuth.AccessToken != "" {
			tok, _ := refreshIfExpiredContext(ctx, "kimi", c.Kimi.OAuth)
			return tok.AccessToken, "oauth", "", nil
		}
		if kimiCLIFallbackDisabled() {
			break
		}
		if tok := loadKimiCodeCLIToken(); tok != nil && tok.AccessToken != "" {
			tok, _ = refreshIfExpiredContext(ctx, "kimi", tok)
			return tok.AccessToken, "oauth", "", nil
		}
	case "github-copilot":
		if c.GithubCopilot.OAuth != nil && c.GithubCopilot.OAuth.AccessToken != "" {
			return c.GithubCopilot.OAuth.AccessToken, "oauth", "", nil
		}
	}
	return "", "", "", fmt.Errorf("no credential for %s", provider)
}

// CredentialAvailable reports whether a provider has a configured credential
// without executing an api_key_command.
func CredentialAvailable(provider string) bool {
	return credentialAvailableEnv(provider, os.Getenv)
}

func credentialAvailableEnv(provider string, getenv func(string) string) bool {
	_, _, _, err := resolveCredentialFullEnv(context.Background(), provider, "", apiKeyCommandAvailable, getenv)
	return err == nil
}

// argsGetenv returns the lookup Resolve uses for provider credentials and
// endpoints: the session env captured in args.Env when set, else the process.
func argsGetenv(args Args) func(string) string {
	if args.Env == nil {
		return os.Getenv
	}
	env := args.Env
	return func(key string) string {
		value := ""
		for _, kv := range env {
			if k, v, ok := strings.Cut(kv, "="); ok && k == key {
				value = v
			}
		}
		return value
	}
}

func resolveCredentialForBackground(ctx context.Context, provider string) (cred, method string, err error) {
	cred, method, _, err = resolveCredentialFull(ctx, provider, "", apiKeyCommandSkip)
	return cred, method, err
}

func storedProviderCreds(c auth.Credentials, provider string) (auth.ProviderCreds, bool) {
	if pc, ok := c.AdditionalAPIKeyCreds[provider]; ok {
		return pc, true
	}
	switch provider {
	case "anthropic":
		return c.Anthropic, true
	case "openai":
		return c.OpenAI, true
	case "kimi":
		return c.Kimi, true
	case "google":
		return c.Google, true
	case "deepseek":
		return c.DeepSeek, true
	case "github-copilot":
		return c.GithubCopilot, true
	default:
		return auth.ProviderCreds{}, false
	}
}

func resolveStoredAPIKey(ctx context.Context, provider string, creds auth.ProviderCreds, mode apiKeyCommandMode) (key string, found bool, err error) {
	if creds.APIKey != "" {
		return creds.APIKey, true, nil
	}
	if creds.APIKeyCommand == nil {
		return "", false, nil
	}
	switch mode {
	case apiKeyCommandAvailable:
		return "<api-key-command>", true, nil
	case apiKeyCommandSkip:
		return "", false, nil
	default:
		key, err := auth.ResolveAPIKeyCommand(ctx, *creds.APIKeyCommand)
		if err != nil {
			return "", true, fmt.Errorf("resolve api key for %s: %w", provider, err)
		}
		return key, true, nil
	}
}

// kimiCLIFallbackDisabledPath returns the path to the sentinel that disables
// fallback to the official Kimi Code CLI token after `zut /logout kimi`.
func kimiCLIFallbackDisabledPath() string {
	return filepath.Join(config.ZutHome(), "kimi-cli-fallback-disabled")
}

func kimiCLIFallbackDisabled() bool {
	_, err := os.Stat(kimiCLIFallbackDisabledPath())
	return err == nil
}

func SetKimiCLIFallbackDisabled(disabled bool) error {
	path := kimiCLIFallbackDisabledPath()
	if !disabled {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("disabled\n"), 0o600)
}

// normalizeCustomProviderEnvVar converts a provider id such as
// "my-company" to "MY_COMPANY", matching the common convention for
// shell environment variables.
func normalizeCustomProviderEnvVar(provider string) string {
	provider = strings.ToUpper(provider)
	provider = strings.ReplaceAll(provider, "-", "_")
	return provider
}

func loadKimiCodeCLIToken() *auth.OAuthToken {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(home, ".kimi", "credentials", "kimi-code.json"))
	if err != nil {
		return nil
	}
	var raw struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		TokenType    string  `json:"token_type"`
		ExpiresAt    float64 `json:"expires_at"`
		Scope        string  `json:"scope"`
		ExpiresIn    float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &raw); err != nil || raw.AccessToken == "" {
		return nil
	}
	sec := int64(raw.ExpiresAt)
	nsec := int64((raw.ExpiresAt - float64(sec)) * 1e9)
	return &auth.OAuthToken{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		TokenType:    raw.TokenType,
		Scope:        raw.Scope,
		ClientID:     auth.KimiOAuth.ClientID,
		Expiry:       time.Unix(sec, nsec),
	}
}

// loadOAuthToken reads the current OAuth token from auth.json for the
// given provider. Returns nil if no token is stored.
func loadOAuthToken(providerName string) *auth.OAuthToken {
	c, err := AuthStoreFor().Load()
	if err != nil {
		return nil
	}
	switch providerName {
	case "anthropic":
		if c.Anthropic.OAuth != nil {
			return c.Anthropic.OAuth
		}
	case "openai":
		if c.OpenAI.OAuth != nil {
			return c.OpenAI.OAuth
		}
	case "kimi":
		if c.Kimi.OAuth != nil {
			return c.Kimi.OAuth
		}
		if kimiCLIFallbackDisabled() {
			return nil
		}
		return loadKimiCodeCLIToken()
	case "github-copilot":
		if c.GithubCopilot.OAuth != nil {
			return c.GithubCopilot.OAuth
		}
	}
	if pc, ok := c.AdditionalAPIKeyCreds[providerName]; ok {
		return pc.OAuth
	}
	return nil
}

// refreshIfExpired returns a usable OAuth token for the given provider,
// refreshing it synchronously when it's past (or near) expiry. The
// refreshed token is persisted to auth.json.
//
// Failures return the original token unchanged — the caller then makes
// a request with the stale access_token, which will 401. That's still
// better than crashing at credential-resolution time.
func refreshIfExpired(providerName string, tok *auth.OAuthToken) (*auth.OAuthToken, error) {
	return refreshIfExpiredContext(context.Background(), providerName, tok)
}

// refreshIfExpiredContext uses the caller's cancellation deadline while
// refreshing an expired OAuth token.
func refreshIfExpiredContext(parent context.Context, providerName string, tok *auth.OAuthToken) (*auth.OAuthToken, error) {
	if tok == nil {
		return &auth.OAuthToken{}, fmt.Errorf("nil token")
	}
	if !tok.Expired() {
		return tok, nil
	}
	if tok.RefreshToken == "" {
		return tok, fmt.Errorf("%s oauth token expired and no refresh_token available — run /login again", providerName)
	}

	if providerName == "xai" {
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		next, err := auth.RefreshXAIToken(ctx, tok.RefreshToken)
		if err != nil {
			return tok, fmt.Errorf("refresh %s: %w", providerName, err)
		}
		if next.RefreshToken == "" {
			next.RefreshToken = tok.RefreshToken
		}
		if err := AuthStoreFor().SetOAuth(providerName, *next); err != nil {
			return next, fmt.Errorf("persist refreshed token: %w", err)
		}
		return next, nil
	}

	var op auth.OAuthProvider
	switch providerName {
	case "anthropic":
		op = auth.AnthropicOAuth
	case "openai":
		op = auth.OpenAIOAuth
	case "kimi":
		op = auth.KimiOAuth
	default:
		return tok, fmt.Errorf("unknown provider %q", providerName)
	}

	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	next, err := op.Refresh(ctx, tok.RefreshToken)
	if err != nil {
		return tok, fmt.Errorf("refresh %s: %w", providerName, err)
	}
	// Preserve the refresh token if the server omitted it (Anthropic often does).
	if next.RefreshToken == "" {
		next.RefreshToken = tok.RefreshToken
	}
	// Carry over account id (openai) / id_token across refreshes.
	if next.AccountID == "" {
		next.AccountID = tok.AccountID
	}
	if next.IDToken == "" {
		next.IDToken = tok.IDToken
	}
	if err := AuthStoreFor().SetOAuth(providerName, *next); err != nil {
		return next, fmt.Errorf("persist refreshed token: %w", err)
	}
	return next, nil
}
