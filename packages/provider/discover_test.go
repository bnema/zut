package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscoverOpenAICodex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		if got := r.URL.Query().Get("client_version"); got != openaiCodexClientVersion {
			t.Errorf("client_version = %q, want %q", got, openaiCodexClientVersion)
		}
		if got := r.Header.Get("authorization"); got != "Bearer token" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("chatgpt-account-id"); got != "account" {
			t.Errorf("chatgpt-account-id = %q", got)
		}
		if got := r.Header.Get("originator"); got != "codex_cli_rs" {
			t.Errorf("originator = %q", got)
		}
		_, _ = io.WriteString(w, `{"models":[
			{"slug":"gpt-5.6-luna","display_name":"GPT-5.6 Luna","supported_in_api":true,"context_window":272000,"max_context_window":400000},
			{"slug":"gpt-hidden","display_name":"Hidden","supported_in_api":false},
			{"slug":"gpt-custom","supported_in_api":true,"max_context_window":100000}
		]}`)
	}))
	defer srv.Close()

	got, err := DiscoverOpenAICodex(context.Background(), "token", "account", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("models = %+v, want 2 models", got)
	}
	if got[0].Provider != "openai-codex" || got[0].ID != "gpt-5.6-luna" || got[0].DisplayName != "GPT-5.6 Luna" || got[0].ContextWindow != 272000 || !got[0].Reasoning || got[0].Source != "live" {
		t.Errorf("first model = %+v", got[0])
	}
	if got[1].ID != "gpt-custom" || got[1].DisplayName != "gpt-custom" || got[1].ContextWindow != 100000 {
		t.Errorf("second model = %+v", got[1])
	}
}

func TestDiscoverOpenAICodexRejectsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := DiscoverOpenAICodex(context.Background(), "token", "", srv.URL); err == nil {
		t.Fatal("DiscoverOpenAICodex error = nil, want HTTP error")
	}
}
