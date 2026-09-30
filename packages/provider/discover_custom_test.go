package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCustomModelsURL(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:48000":          "http://127.0.0.1:48000/v1/models",
		"http://127.0.0.1:48000/":         "http://127.0.0.1:48000/v1/models",
		"http://127.0.0.1:48000/v1":       "http://127.0.0.1:48000/v1/models",
		"http://127.0.0.1:48000/v1/":      "http://127.0.0.1:48000/v1/models",
		"https://llm.example.com/paas/v4": "https://llm.example.com/paas/v4/models",
	}
	for in, want := range cases {
		got, err := customModelsURL(in)
		if err != nil || got != want {
			t.Errorf("customModelsURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "localhost:1234", "ftp://x", "/v1"} {
		if _, err := customModelsURL(bad); err == nil {
			t.Errorf("customModelsURL(%q) accepted", bad)
		}
	}
}

func TestDiscoverCustomProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer dummy" {
			t.Errorf("missing bearer header: %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"object":"list","data":[{"id":"qwen3-coder"},{"id":"nomic-embed-text"},{"id":"qwen3-coder"},{"id":""},{"id":"gpt-oss-20b"}]}`)
	}))
	defer server.Close()

	models, err := DiscoverCustomProvider(context.Background(), "m4", server.URL+"/v1", "dummy")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	first := models[0]
	if first.Provider != "m4" || first.ID != "qwen3-coder" || first.Source != "live" || first.BaseURL != server.URL+"/v1" {
		t.Fatalf("model = %+v", first)
	}
	if first.ContextWindow != customDiscoveryContextWindow || first.MaxOutput != customDiscoveryMaxOutput {
		t.Fatalf("defaults = %d/%d", first.ContextWindow, first.MaxOutput)
	}
	if models[1].ID != "gpt-oss-20b" {
		t.Fatalf("second model = %+v", models[1])
	}
}

func TestDiscoverCustomProviderOmitsHeaderWithoutKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			t.Error("unexpected Authorization header")
		}
		fmt.Fprint(w, `{"data":[{"id":"local"}]}`)
	}))
	defer server.Close()
	models, err := DiscoverCustomProvider(context.Background(), "m5", server.URL, "")
	if err != nil || len(models) != 1 {
		t.Fatalf("models=%+v err=%v", models, err)
	}
}

func TestDiscoverCustomProviderErrors(t *testing.T) {
	status := http.StatusUnauthorized
	body := `{}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer server.Close()

	if _, err := DiscoverCustomProvider(context.Background(), "m4", server.URL, "k"); err == nil || err.Error() != "m4 discovery HTTP 401" {
		t.Fatalf("status error = %v", err)
	}
	status = http.StatusOK
	body = `{"object":"list"}`
	if _, err := DiscoverCustomProvider(context.Background(), "m4", server.URL, "k"); err == nil {
		t.Fatal("missing data accepted")
	}
	body = `not json`
	if _, err := DiscoverCustomProvider(context.Background(), "m4", server.URL, "k"); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if _, err := DiscoverCustomProvider(context.Background(), "", server.URL, "k"); err == nil {
		t.Fatal("empty provider id accepted")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DiscoverCustomProvider(ctx, "m4", server.URL, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestDiscoverCustomProviderBoundsResponseAndUsesClient(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"`)
		chunk := make([]byte, 1<<20)
		for i := range chunk {
			chunk[i] = 'a'
		}
		for i := 0; i < 9; i++ {
			w.Write(chunk)
		}
		fmt.Fprint(w, `"}]}`)
	}))
	defer big.Close()
	if _, err := DiscoverCustomProvider(context.Background(), "m4", big.URL, ""); err == nil {
		t.Fatal("oversized response accepted")
	}

	used := false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		used = true
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"x"}]}`)), Header: http.Header{}}, nil
	})}
	models, err := DiscoverCustomProviderWithClient(context.Background(), client, "m4", "http://example.invalid/v1", "")
	if err != nil || !used || len(models) != 1 {
		t.Fatalf("custom client not used: used=%v models=%v err=%v", used, models, err)
	}
}
