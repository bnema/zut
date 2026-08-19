package tools

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func testWebFetcher(t *testing.T, server *httptest.Server) *webFetcher {
	t.Helper()
	address := server.Listener.Addr().String()
	return &webFetcher{
		resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
}

func TestWebPageToolsNavigateOnlyStoredReferences(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/next" {
			_, _ = w.Write([]byte("<html><body><p>Second page</p></body></html>"))
			return
		}
		_, _ = w.Write([]byte(`<html><head><title>Example</title></head><body><script>ignore</script><main><h1>Heading</h1><p>Useful text</p><a href="/next">Next page</a></main></body></html>`))
	}))
	defer server.Close()

	store := NewBrowsingStore()
	ref := store.addSource("http://public.test/")
	fetcher := testWebFetcher(t, server)
	open := &WebOpenTool{store: store, fetcher: fetcher}
	find := &WebFindTool{store: store}
	click := &WebClickTool{store: store, fetcher: fetcher}

	result, err := open.Execute(context.Background(), json.RawMessage(`{"ref_id":"`+ref+`"}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("open result = %#v, err = %v", result, err)
	}
	text := result.Content[0].(provider.TextBlock).Text
	if !strings.Contains(text, "Page reference: web-") || !strings.Contains(text, "Useful text") || strings.Contains(text, "ignore") || !strings.Contains(text, "[1] Next page") {
		t.Fatalf("unexpected open output:\n%s", text)
	}
	pageRef := strings.Split(strings.Split(text, "Page reference: ")[1], "\n")[0]

	result, err = find.Execute(context.Background(), json.RawMessage(`{"ref_id":"`+pageRef+`","pattern":"useful"}`), nil)
	if err != nil || result.IsError || !strings.Contains(result.Content[0].(provider.TextBlock).Text, "Useful text") {
		t.Fatalf("find result = %#v, err = %v", result, err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("find made %d requests, want 1 total", got)
	}

	result, err = click.Execute(context.Background(), json.RawMessage(`{"ref_id":"`+pageRef+`","id":1}`), nil)
	if err != nil || result.IsError || !strings.Contains(result.Content[0].(provider.TextBlock).Text, "Second page") {
		t.Fatalf("click result = %#v, err = %v", result, err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestWebPageSearchReturnsStoredSourceReference(t *testing.T) {
	store := NewBrowsingStore()
	tool := NewWebSearchToolWithStore(store)
	tool.transport = webSearchRoundTripper(func(request *http.Request) (*http.Response, error) {
		return webSearchHTMLResponse(request, webSearchFixture), nil
	})
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"reference","max_results":1}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("search result = %#v, err = %v", result, err)
	}
	text := result.Content[0].(provider.TextBlock).Text
	if !strings.Contains(text, "(ref: web-") {
		t.Fatalf("search did not expose a source reference: %s", text)
	}
	ref := strings.Split(strings.Split(text, "(ref: ")[1], ")")[0]
	doc, ok := store.get(ref, false)
	if !ok || !doc.source || doc.url != "https://example.com/one" {
		t.Fatalf("stored source = %#v, found = %v", doc, ok)
	}
}

func TestWebSearchDoesNotCommitSourcesAfterStoreRevocation(t *testing.T) {
	store := NewBrowsingStore()
	tool := NewWebSearchToolWithStore(store)
	tool.transport = webSearchRoundTripper(func(request *http.Request) (*http.Response, error) {
		store.Clear()
		return webSearchHTMLResponse(request, webSearchFixture), nil
	})
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"revoked"}`), nil)
	if err != nil || !result.IsError || result.Content[0].(provider.TextBlock).Text != "web search: unavailable in this session" {
		t.Fatalf("revoked search result = %#v, err = %v", result, err)
	}
	if _, ok := store.get("web-1", false); ok {
		t.Fatal("revoked search retained a source reference")
	}
}

func TestWebPageFetcherRedirectsWithFreshValidation(t *testing.T) {
	var resolutions atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("complete"))
	}))
	defer server.Close()
	fetcher := testWebFetcher(t, server)
	fetcher.resolve = func(context.Context, string) ([]netip.Addr, error) {
		resolutions.Add(1)
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	body, finalURL, media, message := fetcher.fetch(context.Background(), "http://public.test/redirect")
	if message != "" || string(body) != "complete" || finalURL != "http://public.test/final" || media != "text/plain" {
		t.Fatalf("redirect result body=%q url=%q media=%q message=%q", body, finalURL, media, message)
	}
	if resolutions.Load() != 2 {
		t.Fatalf("resolutions = %d, want 2", resolutions.Load())
	}
}

func TestWebPageFetcherPreservesHTTPSHostnameForSNI(t *testing.T) {
	var serverName atomic.Value
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverName.Store(r.TLS.ServerName)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("secure"))
	}))
	server.StartTLS()
	defer server.Close()
	fetcher := testWebFetcher(t, server)
	// httptest uses its own generated certificate. The unexported seam is used
	// only to trust it for this local SNI assertion; production never supplies
	// InsecureSkipVerify.
	fetcher.tlsConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only local certificate
	body, _, _, message := fetcher.fetch(context.Background(), "https://public.test/")
	got, _ := serverName.Load().(string)
	if message != "" || string(body) != "secure" || got != "public.test" {
		t.Fatalf("HTTPS body=%q message=%q SNI=%q", body, message, got)
	}
}

func TestWebPageFetcherPinsTheValidatedAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("pinned"))
	}))
	defer server.Close()
	var dialed string
	fetcher := testWebFetcher(t, server)
	originalDial := fetcher.dial
	fetcher.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = address
		return originalDial(ctx, network, address)
	}
	body, _, _, message := fetcher.fetch(context.Background(), "http://public.test/")
	if message != "" || string(body) != "pinned" || dialed != "8.8.8.8:80" {
		t.Fatalf("pinned fetch body=%q message=%q dialed=%q", body, message, dialed)
	}
}

func TestWebPageFetcherRejectsPrivateAndMixedDestinationsBeforeDial(t *testing.T) {
	var dials atomic.Int64
	fetcher := &webFetcher{
		resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
		},
		dial: func(context.Context, string, string) (net.Conn, error) { dials.Add(1); return nil, context.Canceled },
	}
	_, _, _, message := fetcher.fetch(context.Background(), "http://public.test/")
	if message != webPageDenied || dials.Load() != 0 {
		t.Fatalf("mixed destination message=%q dials=%d", message, dials.Load())
	}
	_, _, _, message = fetcher.fetch(context.Background(), "http://127.0.0.1/")
	if message != webPageDenied || dials.Load() != 0 {
		t.Fatalf("literal private destination message=%q dials=%d", message, dials.Load())
	}
}

func TestPublicWebIPRejectsSpecialIPv6Ranges(t *testing.T) {
	for _, raw := range []string{"2001:db8::1", "2001:2::1", "64:ff9b:1::1", "fc00::1"} {
		if publicWebIP(netip.MustParseAddr(raw)) {
			t.Fatalf("special IPv6 address %s was allowed", raw)
		}
	}
	if !publicWebIP(netip.MustParseAddr("2606:4700:4700::1111")) {
		t.Fatal("public IPv6 address was rejected")
	}
}

func TestWebPageParserBudgetAndCancellation(t *testing.T) {
	deep := []byte("<html><body>" + strings.Repeat("<div>", webPageMaxDepth+1) + "text" + strings.Repeat("</div>", webPageMaxDepth+1) + "</body></html>")
	if _, message := parseWebPage(deep, "http://public.test/", "text/html"); message != webPageUnparseable {
		t.Fatalf("deep HTML message = %q, want %q", message, webPageUnparseable)
	}
	fetcher := &webFetcher{
		resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		dial: func(context.Context, string, string) (net.Conn, error) { return nil, context.Canceled },
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, message := fetcher.fetch(ctx, "http://public.test/")
	if message != webPageCancelled {
		t.Fatalf("cancelled fetch message = %q, want %q", message, webPageCancelled)
	}
	fetcher.resolve = func(ctx context.Context, _ string) ([]netip.Addr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, _, _, message = fetcher.fetch(ctx, "http://public.test/")
	if message != webPageCancelled {
		t.Fatalf("cancelled resolver message = %q, want %q", message, webPageCancelled)
	}
}

func TestBrowsingStoreConcurrentAccess(t *testing.T) {
	store := NewBrowsingStore()
	var wait sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 100; iteration++ {
				id := store.addSource("http://public.test/")
				store.get(id, false)
				if iteration%25 == 0 {
					store.Clear()
				}
			}
		}()
	}
	wait.Wait()
}

func TestWebPageStoreGenerationAndSchemaFailures(t *testing.T) {
	store := NewBrowsingStore()
	retained := store.addSource("http://public.test/")
	generation := store.snapshotGeneration()
	if id, committed := store.addPage(&webDocument{url: "http://public.test", lines: []string{strings.Repeat("x", webPageMaxStoreBytes+1)}}, generation); id != "" || committed {
		t.Fatalf("oversized page commit = %q, %v", id, committed)
	}
	if _, ok := store.get(retained, false); !ok {
		t.Fatal("oversized page evicted an existing document")
	}
	store.Clear()
	if id, committed := store.addPage(&webDocument{url: "http://public.test", lines: []string{"text"}}, generation); id != "" || committed {
		t.Fatalf("stale page commit = %q, %v", id, committed)
	}
	find := &WebFindTool{store: store}
	result, err := find.Execute(context.Background(), json.RawMessage(`{"ref_id":"https://not-a-handle","pattern":"x"}`), nil)
	if err != nil || !result.IsError || result.Content[0].(provider.TextBlock).Text != webPageInvalid {
		t.Fatalf("arbitrary URL result = %#v, err = %v", result, err)
	}
}
