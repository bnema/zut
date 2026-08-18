package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAIReportsReasoningTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":6}}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "data: [DONE]")
		fmt.Fprintln(w)
	}))
	defer srv.Close()

	events, err := NewOpenAI("test", srv.URL).Stream(context.Background(), Request{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	usage := usageFromEvents(events)
	if usage.OutputTokens != 9 || usage.ReasoningTokens != 6 || !usage.ReasoningTokensKnown {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestOpenAINormalizesMeasuredCacheUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":70,"cache_write_tokens":10}}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "data: [DONE]")
		fmt.Fprintln(w)
	}))
	defer srv.Close()

	events, err := NewOpenAI("test", srv.URL).Stream(context.Background(), Request{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	usage := usageFromEvents(events)
	if usage.InputTokens != 20 || usage.CacheReadTokens != 70 || usage.CacheWriteTokens != 10 || usage.CacheMeasuredPromptTokens != 100 || usage.CacheMeasuredReadTokens != 70 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestOpenAIResponsesNormalizesMeasuredCacheUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":100,"output_tokens":9,"input_tokens_details":{"cached_tokens":70,"cache_write_tokens":10}}}}`)
		fmt.Fprintln(w)
	}))
	defer srv.Close()

	events, err := NewOpenAICodex("test", "account", srv.URL).Stream(context.Background(), Request{Model: "gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	usage := usageFromEvents(events)
	if usage.InputTokens != 20 || usage.CacheReadTokens != 70 || usage.CacheWriteTokens != 10 || usage.CacheMeasuredPromptTokens != 100 || usage.CacheMeasuredReadTokens != 70 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestOpenAIRejectsMalformedCacheUsageDetails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":70,"cache_write_tokens":40}}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "data: [DONE]")
		fmt.Fprintln(w)
	}))
	defer srv.Close()

	events, err := NewOpenAI("test", srv.URL).Stream(context.Background(), Request{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	usage := usageFromEvents(events)
	if usage.InputTokens != 100 || usage.CacheReadTokens != 0 || usage.CacheWriteTokens != 0 || usage.CacheMeasuredPromptTokens != 0 || usage.CacheMeasuredReadTokens != 0 {
		t.Fatalf("malformed cache usage = %+v", usage)
	}
}

func TestOpenAIResponsesReportsReasoningTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":12,"output_tokens":9,"output_tokens_details":{"reasoning_tokens":6}}}}`)
		fmt.Fprintln(w)
	}))
	defer srv.Close()

	events, err := NewOpenAICodex("test", "account", srv.URL).Stream(context.Background(), Request{Model: "gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	usage := usageFromEvents(events)
	if usage.OutputTokens != 9 || usage.ReasoningTokens != 6 || !usage.ReasoningTokensKnown {
		t.Fatalf("usage = %+v", usage)
	}
}

func usageFromEvents(events <-chan Event) Usage {
	var usage Usage
	for event := range events {
		if e, ok := event.(EventUsage); ok {
			usage = e.Usage
		}
	}
	return usage
}
