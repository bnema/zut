package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/provider/auth"
)

type dictationRoundTripFunc func(*http.Request) (*http.Response, error)

func (f dictationRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDictationCredentials(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OPENAI_CODEX_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "synthetic-api-key")
	if _, _, err := dictationCredentials(context.Background()); !errors.Is(err, modes.ErrDictationLogin) {
		t.Fatal(err)
	}
	store := AuthStoreFor()
	if err := store.SetOAuth("openai", auth.OAuthToken{AccessToken: "synthetic-old", RefreshToken: "synthetic-refresh", AccountID: "synthetic-account", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	old := http.DefaultTransport
	http.DefaultTransport = dictationRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != auth.OpenAIOAuth.TokenURL {
			t.Errorf("unexpected request %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"synthetic-new","expires_in":3600,"token_type":"Bearer"}`))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	token, account, err := dictationCredentials(context.Background())
	if err != nil || token != "synthetic-new" || account != "synthetic-account" || calls != 1 {
		t.Fatalf("refresh failed: account=%q calls=%d err=%v", account, calls, err)
	}
	saved := loadOAuthToken("openai")
	if saved.AccountID != "synthetic-account" || saved.RefreshToken != "synthetic-refresh" {
		t.Fatal("lost account or refresh token")
	}
	t.Setenv("OPENAI_CODEX_API_KEY", "synthetic-shadow")
	if _, _, err := dictationCredentials(context.Background()); !errors.Is(err, modes.ErrDictationLogin) {
		t.Fatal("accepted API key", err)
	}
}
func TestDictationSettings(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	cfg := interactiveDictationConfig(nil)
	if cfg.Recorder.MaxDuration != 600*time.Second {
		t.Fatal("wrong default")
	}
	for _, seconds := range []int{-1, 0, 1801} {
		if err := SaveConfig(Config{Dictation: &DictationSettings{MaxSeconds: &seconds}}); err == nil {
			t.Fatalf("accepted %d", seconds)
		}
	}
	seconds := 120
	s := &DictationSettings{Recorder: []string{"fake-recorder", "{output}"}, MaxSeconds: &seconds, Language: "fr"}
	if err := SaveConfig(Config{Dictation: s}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg = interactiveDictationConfig(loaded.Dictation)
	if cfg.Language != "fr" || cfg.Recorder.MaxDuration != 120*time.Second || len(cfg.Recorder.Command) != 2 {
		t.Fatalf("wrong config %+v", cfg)
	}
}
