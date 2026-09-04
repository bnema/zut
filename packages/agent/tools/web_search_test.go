package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

type webSearchRoundTripper func(*http.Request) (*http.Response, error)

func (f webSearchRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func webSearchDirectTestTransport(t *testing.T) http.RoundTripper {
	t.Helper()
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	return transport
}

func webSearchText(t *testing.T, resultText any) string {
	t.Helper()
	result, ok := resultText.(provider.TextBlock)
	if !ok {
		t.Fatalf("content block = %T, want provider.TextBlock", resultText)
	}
	return result.Text
}

func executeWebSearch(t *testing.T, tool *WebSearchTool, ctx context.Context, args any) (string, bool, any) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return executeWebSearchRaw(t, tool, ctx, raw)
}

func executeWebSearchRaw(t *testing.T, tool *WebSearchTool, ctx context.Context, raw json.RawMessage) (string, bool, any) {
	t.Helper()
	result, err := tool.Execute(ctx, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("content = %#v", result.Content)
	}
	return webSearchText(t, result.Content[0]), result.IsError, result.Details
}

func webSearchHTMLResponse(request *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

const webSearchFixture = `<!doctype html><html><body>
<div class="result"><a class="result__a" href="/l/?uddg=https%3A%2F%2Fexample.com%2Fone">One <b>result</b></a><a class="result__snippet">First <em>snippet</em>.</a></div>
<div class="result"><h2><a href="https://EXAMPLE.com/one">Duplicate</a></h2><span class="result__snippet">Ignored duplicate.</span></div>
<div class="result"><a class="result__a" href="https://example.org/two#fragment">Two</a><div class="result__snippet">Second snippet.</div></div>
</body></html>`

func TestWebSearchDescriptionAvoidsSearchOperators(t *testing.T) {
	description := NewWebSearchTool().Description()
	for _, want := range []string{"ordinary keyword queries", "site:"} {
		if !strings.Contains(description, want) {
			t.Errorf("description = %q, want %q", description, want)
		}
	}
}

func TestWebSearchRequestAndResultContract(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/html/" {
			t.Errorf("path = %q, want /html/", r.URL.Path)
		}
		if got, want := r.URL.RawQuery, "q=Go+JSON-RPC+%E2%9C%93"; got != want {
			t.Errorf("query = %q, want %q", got, want)
		}
		if got := r.Header.Get("User-Agent"); got != webSearchUserAgent {
			t.Errorf("User-Agent = %q", got)
		}
		if got := r.Header.Get("Cookie"); got != "" {
			t.Errorf("Cookie = %q, want none", got)
		}
		w.Header().Set("Set-Cookie", "session=should-not-be-reused")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(webSearchFixture))
	}))
	defer server.Close()

	tool := &WebSearchTool{transport: webSearchDirectTestTransport(t), endpointOverride: server.URL + "/html/?ignored=value"}
	text, isError, details := executeWebSearch(t, tool, context.Background(), map[string]any{
		"query":       "  Go JSON-RPC ✓  ",
		"max_results": 2,
	})
	if isError {
		t.Fatalf("unexpected error: %s", text)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	if !strings.HasPrefix(text, webSearchDisclosure+"\n\n[1] One result\n    https://example.com/one\n    First snippet.") {
		t.Fatalf("unexpected result text:\n%s", text)
	}
	if strings.Contains(text, "#fragment") || !strings.Contains(text, "https://example.org/two") {
		t.Fatalf("result URLs were not normalized:\n%s", text)
	}
	metadata, ok := details.(map[string]any)
	if !ok || metadata["backend"] != "DuckDuckGo HTML" || metadata["query"] != "Go JSON-RPC ✓" {
		t.Fatalf("details = %#v", details)
	}
	if count, ok := metadata["result_count"].(int); !ok || count != 2 {
		t.Fatalf("result_count = %#v", metadata["result_count"])
	}
}

func TestWebSearchEndpointIsFixed(t *testing.T) {
	var gotURL string
	tool := NewWebSearchTool()
	tool.transport = webSearchRoundTripper(func(request *http.Request) (*http.Response, error) {
		gotURL = request.URL.String()
		return webSearchHTMLResponse(request, webSearchFixture), nil
	})
	text, isError, _ := executeWebSearch(t, tool, context.Background(), map[string]any{"query": "fixed"})
	if isError {
		t.Fatalf("unexpected error: %s", text)
	}
	if gotURL != webSearchEndpoint+"?q=fixed" {
		t.Fatalf("request URL = %q, want %q", gotURL, webSearchEndpoint+"?q=fixed")
	}
}

func TestWebSearchUsesConfiguredProxyTransport(t *testing.T) {
	var proxyCalls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		if r.URL.Scheme != "http" || r.URL.Host != "web-search.invalid" {
			t.Errorf("proxy request URL = %q, want absolute web-search.invalid URL", r.URL)
		}
		if got, want := r.URL.Path, "/html/"; got != want {
			t.Errorf("proxy request path = %q, want %q", got, want)
		}
		if got, want := r.URL.RawQuery, "q=environment"; got != want {
			t.Errorf("proxy request query = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(webSearchFixture))
	}))
	defer proxy.Close()

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	t.Cleanup(transport.CloseIdleConnections)
	tool := &WebSearchTool{
		endpointOverride: "http://web-search.invalid/html/",
		transport:        transport,
	}
	text, isError, _ := executeWebSearch(t, tool, context.Background(), map[string]any{"query": "environment"})
	if isError {
		t.Fatalf("unexpected error through configured proxy: %s", text)
	}
	if got := proxyCalls.Load(); got != 1 {
		t.Fatalf("proxy calls = %d, want 1", got)
	}
}

func TestWebSearchExecuteClosesOwnedTransportConnections(t *testing.T) {
	const calls = 3
	closed := make(chan struct{}, calls)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(webSearchFixture))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	server.Start()
	defer server.Close()

	// ProxyFromEnvironment always bypasses localhost. This keeps this regression
	// test local even after the proxy contract test initializes its cached proxy.
	endpoint := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	tool := &WebSearchTool{endpointOverride: endpoint}
	for i := 0; i < calls; i++ {
		text, isError, _ := executeWebSearch(t, tool, context.Background(), map[string]any{"query": "lifecycle"})
		if isError {
			t.Fatalf("call %d returned an error: %s", i+1, text)
		}
	}
	for i := 0; i < calls; i++ {
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatalf("call %d left its transport connection open", i+1)
		}
	}
}

func TestWebSearchSanitizesResultsAndDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<div class=\"result\"><a class=\"result__a\" href=\"https://example.com\">\x1b[31mRed\x1b[0m\x00 title</a><span class=\"result__snippet\">\x1b]0;terminal title\x07Clean\x01 snippet</span></div>"))
	}))
	defer server.Close()

	tool := &WebSearchTool{transport: webSearchDirectTestTransport(t), endpointOverride: server.URL}
	text, isError, details := executeWebSearch(t, tool, context.Background(), map[string]any{"query": "safe\x1b[31m query\x00"})
	if isError {
		t.Fatalf("unexpected error: %s", text)
	}
	if want := webSearchDisclosure + "\n\n[1] Red title\n    https://example.com\n    Clean snippet\n"; text != want {
		t.Fatalf("sanitized text = %q, want %q", text, want)
	}
	metadata, ok := details.(map[string]any)
	if !ok {
		t.Fatalf("details = %#v", details)
	}
	if metadata["query"] != "safe query" {
		t.Fatalf("sanitized query details = %#v", metadata["query"])
	}
	results, ok := metadata["results"].([]webSearchResult)
	if !ok || len(results) != 1 {
		t.Fatalf("sanitized result details = %#v", metadata["results"])
	}
	if results[0].Title != "Red title" || results[0].Snippet != "Clean snippet" {
		t.Fatalf("sanitized result = %#v", results[0])
	}
	if strings.ContainsAny(text+results[0].Title+results[0].Snippet+metadata["query"].(string), "\x00\x01\x1b") {
		t.Fatalf("control character remained in model-visible output")
	}
}

func TestWebSearchRejectsInvalidArgumentsWithoutRequest(t *testing.T) {
	tool := &WebSearchTool{transport: webSearchRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected request")
		return nil, nil
	})}
	cases := []struct {
		name string
		args any
		want string
	}{
		{name: "missing query", args: map[string]any{}, want: "web search: invalid query"},
		{name: "empty query", args: map[string]any{"query": " \t "}, want: "web search: invalid query"},
		{name: "long query", args: map[string]any{"query": strings.Repeat("界", webSearchMaxQueryRunes+1)}, want: "web search: invalid query"},
		{name: "zero results", args: map[string]any{"query": "x", "max_results": 0}, want: "web search: invalid max_results"},
		{name: "too many results", args: map[string]any{"query": "x", "max_results": 11}, want: "web search: invalid max_results"},
		{name: "noninteger results", args: map[string]any{"query": "x", "max_results": "2"}, want: "web search: invalid max_results"},
		{name: "unknown key", args: map[string]any{"query": "x", "unexpected": true}, want: "web search: invalid query"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, isError, _ := executeWebSearch(t, tool, context.Background(), tc.args)
			if !isError || text != tc.want {
				t.Fatalf("result = (%q, %v), want (%q, true)", text, isError, tc.want)
			}
		})
	}

	largeRaw := json.RawMessage(strings.Repeat(" ", webSearchMaxInputBytes) + `{"query":"x"}`)
	text, isError, _ := executeWebSearchRaw(t, tool, context.Background(), largeRaw)
	if !isError || text != "web search: invalid query" {
		t.Fatalf("large raw input result = (%q, %v)", text, isError)
	}
}

func TestWebSearchBackendFailuresAreSanitized(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		content string
		body    string
		want    string
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, content: "text/html", body: "private backend diagnostics", want: "web search: DuckDuckGo returned an unavailable response"},
		{name: "wrong content type", status: http.StatusOK, content: "application/json", body: `{}`, want: "web search: DuckDuckGo returned an unavailable response"},
		{name: "challenge", status: http.StatusOK, content: "text/html", body: "<html>unusual traffic captcha</html>", want: "web search: DuckDuckGo returned an unavailable response"},
		{name: "unparseable", status: http.StatusOK, content: "text/html", body: "<html><body>ordinary page</body></html>", want: "web search: DuckDuckGo returned an unparseable response"},
		{name: "no usable", status: http.StatusOK, content: "text/html", body: `<div class="result"><a class="result__a" href="javascript:alert(1)">bad</a></div>`, want: "web search: no usable HTTP(S) results"},
		{name: "malformed redirect", status: http.StatusOK, content: "text/html", body: `<div class="result"><a class="result__a" href="/l/?uddg=%">bad</a></div>`, want: "web search: no usable HTTP(S) results"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.content)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			tool := &WebSearchTool{transport: webSearchDirectTestTransport(t), endpointOverride: server.URL}
			text, isError, _ := executeWebSearch(t, tool, context.Background(), map[string]any{"query": "test"})
			if !isError || text != tc.want {
				t.Fatalf("result = (%q, %v), want (%q, true)", text, isError, tc.want)
			}
		})
	}
}

func TestWebSearchDoesNotUseCookies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Cookie"); got != "" {
			t.Errorf("Cookie = %q, want none", got)
		}
		w.Header().Set("Set-Cookie", "session=private")
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(webSearchFixture))
	}))
	defer server.Close()

	tool := &WebSearchTool{transport: webSearchDirectTestTransport(t), endpointOverride: server.URL}
	for i := 0; i < 2; i++ {
		text, isError, _ := executeWebSearch(t, tool, context.Background(), map[string]any{"query": "cookie"})
		if isError {
			t.Fatalf("unexpected error: %s", text)
		}
	}
	if client := tool.client(); client.Jar != nil {
		t.Fatalf("client jar = %v, want nil", client.Jar)
	}
}

func TestWebSearchRejectsRedirectAndOversizedResponse(t *testing.T) {
	var followed atomic.Bool
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			followed.Store(true)
		}
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer redirect.Close()
	tool := &WebSearchTool{transport: webSearchDirectTestTransport(t), endpointOverride: redirect.URL}
	text, isError, _ := executeWebSearch(t, tool, context.Background(), map[string]any{"query": "test"})
	if !isError || text != "web search: DuckDuckGo request failed" {
		t.Fatalf("redirect result = (%q, %v)", text, isError)
	}
	if followed.Load() {
		t.Fatal("redirect was followed")
	}

	oversized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(strings.Repeat("x", webSearchMaxBodyBytes+1)))
	}))
	defer oversized.Close()
	tool = &WebSearchTool{transport: webSearchDirectTestTransport(t), endpointOverride: oversized.URL}
	text, isError, _ = executeWebSearch(t, tool, context.Background(), map[string]any{"query": "test"})
	if !isError || text != "web search: DuckDuckGo returned an unavailable response" {
		t.Fatalf("oversized result = (%q, %v)", text, isError)
	}
}

func TestWebSearchCancellationAndTransportFailure(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tool := &WebSearchTool{}
	text, isError, _ := executeWebSearch(t, tool, cancelled, map[string]any{"query": "test"})
	if !isError || text != "web search: request cancelled" {
		t.Fatalf("cancelled result = (%q, %v)", text, isError)
	}

	deadline, deadlineCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer deadlineCancel()
	text, isError, _ = executeWebSearch(t, tool, deadline, map[string]any{"query": "test"})
	if !isError || text != "web search: timed out" {
		t.Fatalf("deadline result = (%q, %v)", text, isError)
	}

	tool = &WebSearchTool{transport: webSearchRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial private proxy failed")
	})}
	text, isError, _ = executeWebSearch(t, tool, context.Background(), map[string]any{"query": "test"})
	if !isError || text != "web search: DuckDuckGo request failed" || strings.Contains(text, "proxy") {
		t.Fatalf("transport result = (%q, %v)", text, isError)
	}
}

func TestWebSearchActiveCancellationAndRequestDeadline(t *testing.T) {
	type asyncResult struct {
		text    string
		isError bool
	}
	call := func(tool *WebSearchTool, ctx context.Context) <-chan asyncResult {
		done := make(chan asyncResult, 1)
		go func() {
			result, err := tool.Execute(ctx, json.RawMessage(`{"query":"active"}`), nil)
			if err != nil || len(result.Content) != 1 {
				done <- asyncResult{text: "unexpected tool error", isError: true}
				return
			}
			block, ok := result.Content[0].(provider.TextBlock)
			if !ok {
				done <- asyncResult{text: "unexpected content", isError: true}
				return
			}
			done <- asyncResult{text: block.Text, isError: result.IsError}
		}()
		return done
	}
	wait := func(t *testing.T, done <-chan asyncResult) asyncResult {
		t.Helper()
		select {
		case result := <-done:
			return result
		case <-time.After(time.Second):
			t.Fatal("web search did not finish after context termination")
			return asyncResult{}
		}
	}

	started := make(chan struct{})
	tool := &WebSearchTool{transport: webSearchRoundTripper(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	done := call(tool, ctx)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("web search request did not start")
	}
	cancel()
	result := wait(t, done)
	if !result.isError || result.text != "web search: request cancelled" {
		t.Fatalf("active cancellation result = (%q, %v)", result.text, result.isError)
	}

	started = make(chan struct{})
	tool = &WebSearchTool{transport: webSearchRoundTripper(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	deadline, deadlineCancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer deadlineCancel()
	done = call(tool, deadline)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("deadline request did not start")
	}
	result = wait(t, done)
	if !result.isError || result.text != "web search: timed out" {
		t.Fatalf("request deadline result = (%q, %v)", result.text, result.isError)
	}
}

func TestWebSearchPreviewIsLocalAndOutputIsBounded(t *testing.T) {
	called := false
	tool := &WebSearchTool{transport: webSearchRoundTripper(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("unexpected request")
	})}
	raw := json.RawMessage(`{"query":"current Go release"}`)
	preview, err := tool.Preview(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if preview.IsError || called {
		t.Fatalf("preview error=%v called=%v", preview.IsError, called)
	}
	if got := webSearchText(t, preview.Content[0]); !strings.Contains(got, "DuckDuckGo HTML") || !strings.Contains(got, "current Go release") {
		t.Fatalf("preview = %q", got)
	}

	results := make([]webSearchResult, webSearchMaxResults)
	for i := range results {
		results[i] = webSearchResult{
			Title:   strings.Repeat("界", webSearchMaxTitleRunes+20),
			URL:     "https://example.com/" + strings.Repeat("a", webSearchMaxURLBytes-32),
			Snippet: strings.Repeat("界", webSearchMaxSnippetRunes+20),
		}
	}
	output, visible, truncated := formatWebSearchResults(results)
	if len(output) > webSearchMaxOutputBytes || visible == 0 || !truncated {
		t.Fatalf("output bounds = len %d, visible %d, truncated %v", len(output), visible, truncated)
	}
	if got := boundWebSearchText(strings.Repeat("界", webSearchMaxTitleRunes+1), webSearchMaxTitleRunes); len([]rune(got)) != webSearchMaxTitleRunes || !strings.HasSuffix(got, "…") {
		t.Fatalf("rune bound = %q", got)
	}
}
