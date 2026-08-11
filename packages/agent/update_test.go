package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestIsDevVersion(t *testing.T) {
	for _, version := range []string{"", "dev", "0.0.0", "0.0.0-dev", "0.0.0-dev (abc123, today)", "0.0.0-dev+local"} {
		if !isDevVersion(version) {
			t.Errorf("isDevVersion(%q) = false, want true", version)
		}
	}
	for _, version := range []string{"0.0.1", "1.0.0", "0.0.0-rc1"} {
		if isDevVersion(version) {
			t.Errorf("isDevVersion(%q) = true, want false", version)
		}
	}
}

type countingRoundTripper struct {
	calls int
}

func (r *countingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls++
	return nil, errors.New("unexpected network request")
}

func TestCheckForUpdateSkipsDevVersionWithoutCacheOrNetwork(t *testing.T) {
	home := t.TempDir()
	transport := &countingRoundTripper{}
	previousTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	info := CheckForUpdate(ctx, home, "0.0.0-dev")
	if info != (UpdateInfo{}) {
		t.Fatalf("development check = %+v, want an empty result", info)
	}
	if _, err := os.Stat(filepath.Join(home, updateCheckFile)); !os.IsNotExist(err) {
		t.Fatalf("development check wrote a cache file: %v", err)
	}
	if transport.calls != 0 {
		t.Fatalf("development check made %d network requests, want 0", transport.calls)
	}
}

func TestVersionLess(t *testing.T) {
	pseudo := "0.3.35-0.20260809165004-9e3c28d4b65"
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{name: "older release", a: "0.3.33", b: "0.3.34", want: true},
		{name: "newer release", a: "0.3.35", b: "0.3.34", want: false},
		{name: "pseudo after prior release", a: pseudo, b: "0.3.34", want: false},
		{name: "pseudo before final release", a: pseudo, b: "0.3.35", want: true},
		{name: "prior release before pseudo", a: "0.3.34", b: pseudo, want: true},
		{name: "decorated build", a: "0.3.33 (abc1234, 2026-08-09)", b: "0.3.34", want: true},
		{name: "invalid current", a: "not-a-version", b: "0.3.34", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionLess(tc.a, tc.b); got != tc.want {
				t.Fatalf("versionLess(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestBuildInfoDoesNotOfferPseudoVersionDowngrade(t *testing.T) {
	current := "0.3.35-0.20260809165004-9e3c28d4b65"
	info := buildInfo(current, "v0.3.34", "https://example.com/release")
	if info.Available {
		t.Fatalf("buildInfo(%q, %q) offered a downgrade", current, info.Latest)
	}
}
