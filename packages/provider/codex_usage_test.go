package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchCodexWeeklyUsage(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       *float64
		wantErr    bool
	}{
		{"secondary weekly", `{"rate_limit":{"primary_window":{"used_percent":12,"limit_window_seconds":18000},"secondary_window":{"used_percent":31,"limit_window_seconds":604800,"reset_at":2000000000}}}`, 200, usagePercent(69), false},
		{"primary weekly", `{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":604800}}}`, 200, usagePercent(100), false},
		{"exhausted", `{"rate_limit":{"secondary_window":{"used_percent":100,"limit_window_seconds":604800}}}`, 200, usagePercent(0), false},
		{"fractional", `{"rate_limit":{"secondary_window":{"used_percent":30.5,"limit_window_seconds":604800}}}`, 200, usagePercent(69.5), false},
		{"absent", `{}`, 200, nil, false},
		{"null", `{"rate_limit":null}`, 200, nil, false},
		{"non weekly secondary", `{"rate_limit":{"secondary_window":{"used_percent":31,"limit_window_seconds":3600}}}`, 200, nil, false},
		{"missing percent", `{"rate_limit":{"secondary_window":{"limit_window_seconds":604800}}}`, 200, nil, true},
		{"negative", `{"rate_limit":{"secondary_window":{"used_percent":-1,"limit_window_seconds":604800}}}`, 200, nil, true},
		{"over limit", `{"rate_limit":{"secondary_window":{"used_percent":101,"limit_window_seconds":604800}}}`, 200, nil, true},
		{"invalid json", `private-response`, 200, nil, true},
		{"oversized", strings.Repeat(" ", 1<<20) + `{}`, 200, nil, true},
		{"unauthorized", `private-response`, 401, nil, true},
		{"rate limited", `private-response`, 429, nil, true},
		{"server error", `private-response`, 503, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/backend-api/wham/usage" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("ChatGPT-Account-Id") != "synthetic-account" || r.Header.Get("User-Agent") != "codex-cli" {
					t.Error("missing subscription headers")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			got, err := fetchCodexWeeklyUsage(context.Background(), srv.Client(), srv.URL+"/backend-api/wham/usage", "synthetic-token", "synthetic-account")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %t", err, tc.wantErr)
			}
			if err != nil && (strings.Contains(err.Error(), "synthetic") || strings.Contains(err.Error(), "private-response")) {
				t.Fatal("error leaked sensitive data")
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("unexpected usage: %+v", got)
				}
				return
			}
			if got == nil || got.RemainingPercent != *tc.want {
				t.Fatalf("usage = %+v, want %v", got, *tc.want)
			}
			if tc.name == "secondary weekly" && !got.ResetsAt.Equal(time.Unix(2000000000, 0)) {
				t.Fatalf("reset = %v", got.ResetsAt)
			}
		})
	}
}

func usagePercent(n float64) *float64 { return &n }

func TestFetchCodexWeeklyUsageCancellation(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := fetchCodexWeeklyUsage(ctx, srv.Client(), srv.URL, "synthetic-token", "synthetic-account")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation error")
		}
	case <-time.After(time.Second):
		t.Fatal("request ignored cancellation")
	}
}

func TestFetchCodexWeeklyUsageRejectsRedirectsAndMissingCredentials(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/private-target", http.StatusFound)
	}))
	defer srv.Close()
	for _, creds := range [][2]string{{"", "account"}, {"token", ""}} {
		if _, err := fetchCodexWeeklyUsage(context.Background(), srv.Client(), srv.URL, creds[0], creds[1]); err == nil {
			t.Fatal("missing credentials accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("missing credentials made a request")
	}
	if _, err := fetchCodexWeeklyUsage(context.Background(), srv.Client(), srv.URL, "token", "account"); err == nil {
		t.Fatal("redirect accepted")
	}
	if calls.Load() != 1 {
		t.Fatalf("redirect followed: %d requests", calls.Load())
	}
}
