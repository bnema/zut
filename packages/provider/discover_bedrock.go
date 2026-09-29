package provider

// Amazon Bedrock live model discovery.
//
// Enumerates the foundation models and cross-region inference profiles
// available to the caller's AWS credentials, using the Bedrock control
// plane (bedrock.{region}.amazonaws.com), SigV4-signed with the same
// signer as the runtime client (packages/provider/amazon_bedrock.go):
//
//   - ListFoundationModels: the base foundation-model IDs.
//   - ListInferenceProfiles: the us./eu./global./... prefixed IDs zot
//     actually invokes for on-demand throughput.
//
// IMPORTANT auth note: the runtime bearer token (AWS_BEARER_TOKEN_BEDROCK,
// scoped to bedrock:CallWithBearerToken) cannot call these control-plane
// APIs. Discovery therefore requires SigV4 credentials (env keys, an
// AWS_PROFILE, or CLI-resolved SSO/assume-role creds). When only a
// bearer token is present, discovery is skipped and the built-in catalog
// remains the source of truth.
//
// Pricing is intentionally NOT fetched. The AWS Price List API
// (GetProducts, ServiceCode=AmazonBedrock) was evaluated against live
// data and found to carry only stale, input-only rows for a handful of
// legacy models (Claude 2.x / 3 Sonnet / 3 Haiku) with no output or
// cache prices for any current model. See aws-cli issue #9567. The
// hand-maintained catalog in catalog_builtin.go stays authoritative for
// prices, context windows, and capability flags; MergeCatalog keeps that
// metadata when a discovered ID matches a catalog entry, and unknown
// discovered IDs surface in the picker with placeholder prices.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// bytesReader returns a reader for payload, or nil when payload is nil
// so GET requests carry no body.
func bytesReader(payload []byte) io.Reader {
	if payload == nil {
		return nil
	}
	return bytes.NewReader(payload)
}

// DiscoverBedrock lists the foundation models and inference profiles
// available to the caller's AWS credentials in the given region.
//
// SigV4 credentials are resolved from the standard AWS environment /
// profile / CLI sources, even when a bearer token is used for inference.
//
// region defaults to AWS_REGION / AWS_DEFAULT_REGION / us-east-1 when
// empty. A nil error with a nil slice means "no SigV4 credentials" or
// "no models"; callers should treat that as a skip.
func DiscoverBedrock(ctx context.Context, region string) ([]Model, error) {
	if region == "" {
		region = bedrockResolveRegion()
	}
	sigv4 := resolveBedrockDiscoveryCreds()
	if sigv4 == nil {
		// Bearer-only or unauthenticated: control-plane APIs are
		// unreachable. Skip quietly so the catalog stands.
		return nil, nil
	}

	client := &http.Client{Timeout: 15 * time.Second}
	ids, err := bedrockListModelIDs(ctx, client, sigv4, region, "https://bedrock."+region+".amazonaws.com")
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	out := make([]Model, 0, len(ids))
	for _, id := range ids {
		out = append(out, bedrockDiscoveredModel(id, region))
	}
	return out, nil
}

// bedrockDiscoveredModel builds the Model for a discovered ID. The
// control plane returns IDs only, so limits and capabilities come from
// the static catalog. MergeCatalog already uses an exact catalog match;
// this handles IDs without one, most often a geo-prefixed inference
// profile (apac., us-gov., ...) whose base model is catalogued. Without
// this the model would carry MaxOutput 0 and the Bedrock client would
// cap every response at its 4096-token fallback.
func bedrockDiscoveredModel(id, region string) Model {
	m := Model{
		Provider: "amazon-bedrock",
		ID:       id,
		Source:   "live",
		BaseURL:  "https://bedrock-runtime." + region + ".amazonaws.com",
	}
	if bedrockInCatalog(id) {
		// MergeCatalog keeps the exact catalog entry's metadata.
		return m
	}
	prefix, base := bedrockSplitGeoPrefix(id)
	if c, ok := bedrockCatalogBase(base); ok {
		m.ContextWindow = c.ContextWindow
		m.MaxOutput = c.MaxOutput
		m.Reasoning = c.Reasoning
		m.ReasoningLevelMap = maps.Clone(c.ReasoningLevelMap)
		m.AdaptiveThinking = c.AdaptiveThinking
		m.PriceInput = c.PriceInput
		m.PriceOutput = c.PriceOutput
		m.PriceCacheRead = c.PriceCacheRead
		m.PriceCacheWrite = c.PriceCacheWrite
		m.PriceTierInputTokens = c.PriceTierInputTokens
		m.PriceInputAbove = c.PriceInputAbove
		m.PriceOutputAbove = c.PriceOutputAbove
		m.PriceCacheReadAbove = c.PriceCacheReadAbove
		m.PriceCacheWriteAbove = c.PriceCacheWriteAbove
		if c.DisplayName != "" {
			m.DisplayName = c.DisplayName
			if cp, _ := bedrockSplitGeoPrefix(c.ID); cp != "" {
				suffix := " (" + cp + ")"
				if strings.HasSuffix(strings.ToUpper(m.DisplayName), strings.ToUpper(suffix)) {
					m.DisplayName = m.DisplayName[:len(m.DisplayName)-len(suffix)]
				}
			}
			if prefix != "" {
				m.DisplayName += " (" + strings.ToUpper(prefix) + ")"
			}
		}
		return m
	}
	// Unknown models have no verified output or context limits. Keep the
	// client's 4096-token fallback rather than guessing from the family:
	// even Claude Opus 4 supports only 32k, not the 64k of newer models.
	return m
}

// bedrockSplitGeoPrefix separates a cross-region inference-profile geo
// prefix from the foundation-model ID. prefix is "" when there is none.
func bedrockSplitGeoPrefix(id string) (prefix, base string) {
	for _, p := range bedrockGeoPrefixes {
		if strings.HasPrefix(id, p+".") {
			return p, id[len(p)+1:]
		}
	}
	return "", id
}

func bedrockInCatalog(id string) bool {
	for _, c := range Catalog {
		if c.Provider == "amazon-bedrock" && c.ID == id {
			return true
		}
	}
	return false
}

// bedrockCatalogBase finds catalog metadata for a foundation-model ID:
// the bare entry if present, otherwise any geo-prefixed variant of it.
func bedrockCatalogBase(base string) (Model, bool) {
	var variant Model
	found := false
	for _, c := range Catalog {
		if c.Provider != "amazon-bedrock" {
			continue
		}
		if c.ID == base {
			return c, true
		}
		if !found {
			if _, b := bedrockSplitGeoPrefix(c.ID); b == base {
				variant, found = c, true
			}
		}
	}
	return variant, found
}

// bedrockListModelIDs enumerates foundation-model IDs and inference-
// profile IDs available in the region. Profile IDs are the geo-prefixed
// forms zot invokes; both are returned so the picker shows every
// selectable variant. controlPlaneBase is the scheme+host (no trailing
// slash) of the Bedrock control plane, parameterized for testing.
func bedrockListModelIDs(ctx context.Context, client *http.Client, creds *bedrockSigV4Creds, region, controlPlaneBase string) ([]string, error) {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}

	// ListFoundationModels: GET, no pagination token in the response.
	fmURL := controlPlaneBase + "/foundation-models"
	body, err := bedrockSignedControlPlane(ctx, client, creds, region, http.MethodGet, fmURL, nil)
	if err != nil {
		return nil, err
	}
	var fm struct {
		ModelSummaries []struct {
			ModelID          string   `json:"modelId"`
			OutputModalities []string `json:"outputModalities"`
		} `json:"modelSummaries"`
	}
	if err := json.Unmarshal(body, &fm); err != nil {
		return nil, fmt.Errorf("bedrock discover: parse foundation-models: %w", err)
	}
	for _, s := range fm.ModelSummaries {
		// Keep text-output models only; skip embeddings/image generators.
		if !bedrockEmitsText(s.OutputModalities) {
			continue
		}
		add(s.ModelID)
	}

	// ListInferenceProfiles: GET, paginated via ?nextToken=.
	next := ""
	for {
		query := url.Values{"maxResults": {"1000"}}
		if next != "" {
			query.Set("nextToken", next)
		}
		ipURL := controlPlaneBase + "/inference-profiles?" + query.Encode()
		ipBody, err := bedrockSignedControlPlane(ctx, client, creds, region, http.MethodGet, ipURL, nil)
		if err != nil {
			// Non-fatal: foundation models already gathered.
			break
		}
		var ip struct {
			InferenceProfileSummaries []struct {
				InferenceProfileID string `json:"inferenceProfileId"`
			} `json:"inferenceProfileSummaries"`
			NextToken string `json:"nextToken"`
		}
		if err := json.Unmarshal(ipBody, &ip); err != nil {
			break
		}
		for _, s := range ip.InferenceProfileSummaries {
			add(s.InferenceProfileID)
		}
		if ip.NextToken == "" {
			break
		}
		next = ip.NextToken
	}

	return ids, nil
}

func bedrockEmitsText(modalities []string) bool {
	if len(modalities) == 0 {
		return true // Unknown: assume usable rather than hide it.
	}
	for _, m := range modalities {
		if strings.EqualFold(m, "TEXT") {
			return true
		}
	}
	return false
}

// bedrockSignedControlPlane issues a SigV4-signed request to a Bedrock
// control-plane endpoint and returns the response body. Non-2xx is an
// error carrying the status and trimmed body.
func bedrockSignedControlPlane(ctx context.Context, client *http.Client, creds *bedrockSigV4Creds, region, method, url string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytesReader(payload))
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("content-type", "application/json")
	}
	if err := signSigV4(req, payload, "bedrock", region, creds, time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("bedrock discover: sign: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bedrock discover: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bedrock discover: http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}
