package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccountSwitchClearsRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"access_token": "account-b", "token_type": "Bearer"})
	}))
	defer srv.Close()
	for _, grant := range []string{"authorization_code", "refresh_token"} {
		t.Run(grant, func(t *testing.T) {
			e := Entry{AccessToken: "account-a", RefreshToken: "account-a-refresh", TokenEndpoint: srv.URL}
			if err := exchange(context.Background(), Client(nil), &e, url.Values{"grant_type": {grant}}); err != nil {
				t.Fatal(err)
			}
			want := ""
			if grant == "refresh_token" {
				want = "account-a-refresh"
			}
			if e.AccessToken != "account-b" || e.RefreshToken != want {
				t.Fatal("incorrect token ownership", e.RefreshToken)
			}
		})
	}
}

func TestPathSpecificResourceDiscovery(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/mcp/nested", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp/nested", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"resource": srv.URL + "/mcp/nested", "authorization_servers": []string{srv.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		t.Error("root requested before path-specific metadata")
		w.WriteHeader(404)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": srv.URL, "authorization_endpoint": srv.URL + "/authorize", "token_endpoint": srv.URL + "/token"})
	})
	if _, err := discover(context.Background(), Client(nil), srv.URL+"/mcp/nested"); err != nil {
		t.Fatal(err)
	}
}

func TestCancelledKeyLockWaiter(t *testing.T) {
	s := Store{Path: filepath.Join(t.TempDir(), "mcp-auth.json")}
	key := "https://example.invalid/mcp"
	lock := keyLock(s.Path + key)
	if err := lock.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer lock.release()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() { close(started); _, err := (&transport{store: s, key: key}).token(ctx, "", false); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

func TestErrorsPreserveCausesWithoutGuidanceOrSecrets(t *testing.T) {
	cause := errors.New("network synthetic-secret https://example.invalid/?code=synthetic")
	for _, underlying := range []error{cause, context.Canceled, context.DeadlineExceeded} {
		c := Client(&http.Client{Transport: failingTransport{underlying}})
		err := jsonRequest(context.Background(), c, "POST", "https://example.invalid/?code=synthetic", "", nil, &struct{}{})
		if !errors.Is(err, underlying) || strings.Contains(err.Error(), "synthetic") {
			t.Fatal(err)
		}
		s := Store{Path: filepath.Join(t.TempDir(), "auth.json")}
		c = HTTPClient(&http.Client{Transport: failingTransport{underlying}}, s, "https://example.invalid/mcp", "test")
		_, err = c.Get("https://example.invalid/mcp")
		if !errors.Is(err, underlying) || strings.Contains(err.Error(), "zut mcp login") || strings.Contains(err.Error(), "synthetic") {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Login(ctx, Store{Path: filepath.Join(t.TempDir(), "auth.json")}, "https://example.invalid/mcp", io.Discard, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRefreshRetryReplaysPOSTAndMCPHeaders(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"access_token": "fresh", "token_type": "Bearer"})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"method":"tools/call"}` || r.Method != "POST" || r.Header.Get("Mcp-Session-Id") != "session" || r.Header.Get("Mcp-Protocol-Version") != "2025-06-18" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("request not preserved")
		}
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	s := Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	key := srv.URL + "/mcp"
	if err := s.Set(key, &Entry{AccessToken: "old", RefreshToken: "refresh", Resource: key, TokenEndpoint: srv.URL + "/token"}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", key, strings.NewReader(`{"method":"tools/call"}`))
	req.Header.Set("Mcp-Session-Id", "session")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	req.Header.Set("Content-Type", "application/json")
	resp, err := HTTPClient(nil, s, key, "test").Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls != 2 {
		t.Fatal(calls)
	}
}

// Helper communicates over pipes, not timing, and exercises a separate OS lock owner.
func TestStoreHelperProcess(t *testing.T) {
	if os.Getenv("ZUT_OAUTH_HELPER") != "1" {
		return
	}
	s := Store{Path: os.Getenv("ZUT_OAUTH_STORE")}
	f, err := os.OpenFile(s.Path+".lock", os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := tryFileLock(f)
	if acquired {
		unlockFile(f)
	}
	f.Close()
	if err != nil || acquired {
		t.Fatal("parent's file lock did not exclude helper", err)
	}
	if _, err := os.Stdout.Write([]byte("ready\n")); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := os.Stdin.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ZUT_OAUTH_REFRESH") == "1" {
		e, err := (&transport{store: s, key: os.Getenv("ZUT_OAUTH_KEY")}).token(context.Background(), "old", true)
		if err != nil || e.AccessToken != "fresh" {
			t.Fatal("helper refresh", err)
		}
		return
	}
	var e *Entry
	if os.Getenv("ZUT_OAUTH_DELETE") != "1" {
		e = &Entry{AccessToken: "helper"}
	}
	if err := s.Set(os.Getenv("ZUT_OAUTH_KEY"), e); err != nil {
		t.Fatal(err)
	}
}

func startStoreHelper(t *testing.T, s Store, key string, remove bool, extraEnv ...string) (func(), func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreHelperProcess$")
	cmd.Env = append(os.Environ(), "ZUT_OAUTH_HELPER=1", "ZUT_OAUTH_STORE="+s.Path, "ZUT_OAUTH_KEY="+key)
	if remove {
		cmd.Env = append(cmd.Env, "ZUT_OAUTH_DELETE=1")
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var ready [6]byte
	if _, err := io.ReadFull(out, ready[:]); err != nil || string(ready[:]) != "ready\n" {
		t.Fatal("helper readiness", err)
	}
	return func() {
			if _, err := in.Write([]byte("x")); err != nil {
				t.Error(err)
			}
			in.Close()
		}, func() {
			io.Copy(io.Discard, out)
			if err := cmd.Wait(); err != nil {
				t.Error(err)
			}
		}
}

func TestCrossProcessWritesPreserveDifferentKeys(t *testing.T) {
	s := Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	unlock, err := s.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	start, wait := startStoreHelper(t, s, "other", false)
	start()
	if err := s.set("first", &Entry{AccessToken: "first"}); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	wait()
	for _, key := range []string{"first", "other"} {
		e, err := s.Get(key)
		if err != nil || e.AccessToken == "" {
			t.Fatal("lost entry", key, err)
		}
	}
	st, err := os.Stat(s.Path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 && os.PathSeparator != '\\' {
		t.Fatal(st.Mode())
	}
}

func TestRefreshVersusCrossProcessLogout(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		json.NewEncoder(w).Encode(map[string]string{"access_token": "fresh", "token_type": "Bearer"})
	}))
	defer srv.Close()
	s := Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	key := srv.URL + "/mcp"
	if err := s.Set(key, &Entry{AccessToken: "old", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Hour), Resource: key, TokenEndpoint: srv.URL}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := (&transport{store: s, key: key}).token(context.Background(), "", false); done <- err }()
	<-entered
	start, wait := startStoreHelper(t, s, key, true)
	start()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	wait()
	e, err := s.Get(key)
	if err != nil || e.AccessToken != "" {
		t.Fatal("logout resurrected", err)
	}
	e, err = (&transport{store: s, key: key}).token(context.Background(), "old", true)
	if err != nil || e.AccessToken != "" {
		t.Fatal("refresh resurrected logout", err)
	}
}

func TestCrossProcessRefreshUsesNewerToken(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "token_type": "Bearer", "expires_in": 3600})
	}))
	defer srv.Close()
	s := Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	key := srv.URL + "/mcp"
	if err := s.Set(key, &Entry{AccessToken: "old", RefreshToken: "refresh", Resource: key, TokenEndpoint: srv.URL}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := (&transport{store: s, key: key}).token(context.Background(), "old", true)
		done <- err
	}()
	<-entered
	start, wait := startStoreHelper(t, s, key, false, "ZUT_OAUTH_REFRESH=1")
	start()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	wait()
	if calls != 1 {
		t.Fatal("duplicate exchange", calls)
	}
}

func TestRefreshFailurePreservesCauseAndClassifiesGuidance(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	network := errors.New("synthetic-network-secret")
	for _, underlying := range []error{network, context.Canceled, context.DeadlineExceeded} {
		http.DefaultTransport = failingTransport{underlying}
		s := Store{Path: filepath.Join(t.TempDir(), "auth.json")}
		key := "https://example.invalid/mcp"
		if err := s.Set(key, &Entry{AccessToken: "old", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Hour), TokenEndpoint: "https://example.invalid/token"}); err != nil {
			t.Fatal(err)
		}
		_, err := HTTPClient(nil, s, key, "test").Get(key)
		if !errors.Is(err, underlying) || strings.Contains(err.Error(), "zut mcp login") || strings.Contains(err.Error(), "secret") {
			t.Fatal(err)
		}
	}
	http.DefaultTransport = original
	for _, status := range []int{http.StatusUnauthorized, http.StatusBadRequest, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			if status == http.StatusBadRequest {
				io.WriteString(w, `{"error":"invalid_grant","error_description":"synthetic-secret"}`)
			} else {
				io.WriteString(w, "synthetic-secret")
			}
		}))
		s := Store{Path: filepath.Join(t.TempDir(), "auth.json")}
		key := srv.URL + "/mcp"
		if err := s.Set(key, &Entry{AccessToken: "old", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Hour), TokenEndpoint: srv.URL}); err != nil {
			t.Fatal(err)
		}
		_, err := HTTPClient(nil, s, key, "test").Get(key)
		srv.Close()
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "zut mcp login") != (status != http.StatusInternalServerError) {
			t.Fatal(status, err)
		}
	}
}
