package provider

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestModelLookupReportsCatalogCause(t *testing.T) {
	snapshot := SnapshotCatalog()
	t.Cleanup(func() { RestoreCatalog(snapshot) })
	ClearLiveModelsForProvider(ProviderOpenCodeGo)
	for _, tc := range []struct {
		name   string
		status CatalogStatus
		want   string
	}{
		{"missing key", CatalogStatus{State: CatalogMissingCredentials}, "no API key available in this process"},
		{"credential error", CatalogStatus{State: CatalogCredentialError}, "could not load provider credentials"},
		{"deferred", CatalogStatus{State: CatalogCredentialsDeferred}, "credential command not executed"},
		{"unavailable", CatalogStatus{State: CatalogUnavailable}, "no usable model catalog"},
		{"pending", CatalogStatus{State: CatalogRefreshing}, "discovery in progress"},
		{"cached", CatalogStatus{State: CatalogCached}, "absent from the cached"},
		{"published", CatalogStatus{State: CatalogReady}, "absent from the published"},
		{"dns", CatalogStatus{State: CatalogFailed, Failure: DiscoveryError{Kind: DiscoveryDNS}}, "DNS resolution failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			SetProviderCatalogStatus(ProviderOpenCodeGo, tc.status)
			_, err := FindModel(ProviderOpenCodeGo, "unlisted-test-model")
			var lookup *ModelLookupError
			if !errors.As(err, &lookup) || lookup.Catalog.State != tc.status.State || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("lookup error = %v, want typed cause containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "unknown model") {
				t.Fatal("known catalog state lost behind unknown model")
			}
		})
	}
}

func TestCatalogWarningsPreserveUsableModelsAndSnapshot(t *testing.T) {
	snapshot := SnapshotCatalog()
	t.Cleanup(func() { RestoreCatalog(snapshot) })
	SetLiveModelsForProviders([]Model{{Provider: ProviderOpenCodeGo, ID: "cached-test-model"}}, []string{ProviderOpenCodeGo})
	SetProviderCatalogStatus(ProviderOpenCodeGo, CatalogStatus{State: CatalogFailed, Failure: DiscoveryError{Kind: DiscoveryTimeout}})
	if _, err := FindModel(ProviderOpenCodeGo, "cached-test-model"); err != nil {
		t.Fatal(err)
	}
	if warnings := strings.Join(CatalogWarnings(), "\n"); !strings.Contains(warnings, "timed out") || !strings.Contains(warnings, "previously loaded models remain available") {
		t.Fatalf("warnings = %s", warnings)
	}
	failed := SnapshotCatalog()
	SetProviderCatalogStatus(ProviderOpenCodeGo, CatalogStatus{State: CatalogReady})
	if warnings := CatalogWarnings(); len(warnings) != 0 {
		t.Fatalf("successful discovery retained warnings: %v", warnings)
	}
	RestoreCatalog(failed)
	if ProviderCatalogStatus(ProviderOpenCodeGo).State != CatalogFailed {
		t.Fatal("snapshot lost diagnostic state")
	}
}

func TestMissingCredentialWarningOnlyForKnownProvider(t *testing.T) {
	snapshot := SnapshotCatalog()
	t.Cleanup(func() { RestoreCatalog(snapshot) })
	SetProviderCatalogStatus(ProviderOpenCodeGo, CatalogStatus{State: CatalogMissingCredentials})
	if warnings := CatalogWarnings(); len(warnings) != 0 {
		t.Fatalf("unsolicited warning for unused provider: %v", warnings)
	}
	if warnings := CatalogWarnings(ProviderOpenCodeGo); len(warnings) != 1 {
		t.Fatalf("saved provider disappeared: %v", warnings)
	}
	SetProviderCatalogStatus(ProviderOpenCodeGo, CatalogStatus{State: CatalogMissingCredentials, PreviouslyDiscovered: true})
	if warnings := CatalogWarnings(); len(warnings) != 1 {
		t.Fatalf("cached provider disappeared: %v", warnings)
	}
}

func TestDiscoveryErrorClassificationDoesNotLeakTransportDetails(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want DiscoveryFailure
	}{
		{&url.Error{Op: "Get", URL: "https://synthetic-secret@example.test/private", Err: &net.DNSError{Name: "private-host", Err: "synthetic-secret"}}, DiscoveryDNS},
		{context.DeadlineExceeded, DiscoveryTimeout},
		{context.Canceled, DiscoveryCanceled},
		{errors.New("synthetic-secret"), DiscoveryNetwork},
	} {
		got := ClassifyDiscoveryError(tc.err)
		if got.Kind != tc.want || strings.Contains(got.Error(), "synthetic-secret") || strings.Contains(got.Error(), "private-host") {
			t.Fatalf("classification = %+v", got)
		}
	}
}

func TestOpenCodeDiscoveryClassifiesHTTPAndInvalidCatalog(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
		kind DiscoveryFailure
		want string
	}{
		{"unauthorized", 401, "synthetic-secret", DiscoveryHTTP, "authentication rejected"},
		{"forbidden", 403, "synthetic-secret", DiscoveryHTTP, "access denied"},
		{"rate limit", 429, "synthetic-secret", DiscoveryHTTP, "rate limited"},
		{"server", 503, "synthetic-secret", DiscoveryHTTP, "HTTP 503"},
		{"invalid JSON", 200, "synthetic-secret", DiscoveryInvalidResponse, "invalid catalog response"},
		{"missing data", 200, `{}`, DiscoveryInvalidResponse, "invalid catalog response"},
		{"null data", 200, `{"data":null}`, DiscoveryInvalidResponse, "invalid catalog response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/metadata" {
					_, _ = w.Write([]byte(`{}`))
					return
				}
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			_, err := discoverOpenCodeGo(context.Background(), "synthetic-key", server.URL, server.URL+"/metadata")
			var failure *DiscoveryError
			if !errors.As(err, &failure) || failure.Kind != tc.kind || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("discovery = %v, want %s", err, tc.want)
			}
			if strings.Contains(err.Error(), "synthetic-secret") || strings.Contains(err.Error(), server.URL) {
				t.Fatal("discovery leaked response or URL")
			}
		})
	}
}
