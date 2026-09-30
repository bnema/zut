package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Conservative metadata for models that a list endpoint describes by ID
// only. Explicit models.json entries override these values.
const (
	customDiscoveryContextWindow = 32768
	customDiscoveryMaxOutput     = 4096
	customDiscoveryTimeout       = 3 * time.Second
	customDiscoveryMaxBody       = 8 << 20
)

// customModelsURL derives the list endpoint from a custom provider base URL.
// A base that already ends in an API version segment such as "/v1" gets
// "/models" appended; a bare host gets "/v1/models".
func customModelsURL(baseURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("custom provider base URL must be an http or https URL")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	base := u.String()
	if versionSegmentSuffix.MatchString(base) {
		return base + "/models", nil
	}
	return base + "/v1/models", nil
}

// DiscoverCustomProvider lists models from a user-defined provider's
// OpenAI-style /models endpoint using the process default HTTP transport, so
// configured proxy settings apply. Pass an empty apiKey for keyless endpoints:
// no Authorization header is sent.
func DiscoverCustomProvider(ctx context.Context, providerID, baseURL, apiKey string) ([]Model, error) {
	return DiscoverCustomProviderWithClient(ctx, nil, providerID, baseURL, apiKey)
}

// DiscoverCustomProviderWithClient is DiscoverCustomProvider with a caller
// supplied HTTP client, for example one from NewHTTPClient when the user scoped
// insecure TLS to custom endpoints. A nil client uses the default transport.
// The request is bounded by a short timeout and an 8 MiB response cap
// regardless of the client's own settings.
//
// Only IDs are trusted from the response; context and output limits use
// conservative defaults because self-hosted gateways rarely report them
// consistently. Entries whose ID mentions embeddings are skipped. Discovered
// models carry the provider's base URL so they route like models.json entries.
func DiscoverCustomProviderWithClient(ctx context.Context, client *http.Client, providerID, baseURL, apiKey string) ([]Model, error) {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return nil, fmt.Errorf("custom provider discovery requires a provider id")
	}
	endpoint, err := customModelsURL(baseURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, customDiscoveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The transport error can echo the URL; keep the message generic.
		return nil, fmt.Errorf("%s discovery request failed", providerID)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s discovery HTTP %d", providerID, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, customDiscoveryMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("%s discovery response could not be read", providerID)
	}
	if len(body) > customDiscoveryMaxBody {
		return nil, fmt.Errorf("%s discovery response too large", providerID)
	}
	var page struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("%s discovery returned invalid model data", providerID)
	}
	if page.Data == nil {
		return nil, fmt.Errorf("%s discovery response is missing data", providerID)
	}
	models := make([]Model, 0, len(page.Data))
	seen := map[string]bool{}
	for _, d := range page.Data {
		id := strings.TrimSpace(d.ID)
		if id == "" || seen[id] || strings.Contains(strings.ToLower(id), "embed") {
			continue
		}
		seen[id] = true
		models = append(models, Model{
			Provider:      providerID,
			ID:            id,
			DisplayName:   id,
			ContextWindow: customDiscoveryContextWindow,
			MaxOutput:     customDiscoveryMaxOutput,
			BaseURL:       strings.TrimRight(strings.TrimSpace(baseURL), "/"),
			Source:        "live",
		})
	}
	return models, nil
}
