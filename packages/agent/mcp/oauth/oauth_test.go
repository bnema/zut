package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStorageAndPolicy(t *testing.T) {
	s := Store{Path: filepath.Join(t.TempDir(), "private", "mcp-auth.json")}
	key, err := Canonical("https://EXAMPLE.com/mcp?q=private#x")
	if err != nil || key != "https://example.com/mcp" {
		t.Fatal(key, err)
	}
	if err = s.Set(key, &Entry{AccessToken: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, p := range []struct {
			path string
			mode os.FileMode
		}{{s.Path, 0600}, {filepath.Dir(s.Path), 0700}} {
			st, err := os.Stat(p.path)
			if err != nil || st.Mode().Perm() != p.mode {
				t.Fatal(st, err)
			}
		}
	}
	for _, tc := range []struct {
		url  string
		want bool
	}{{"https://example.com/mcp", true}, {"https://example.com/mcp/tools", true}, {"https://example.com/mcprivate", false}, {"https://other.invalid/mcp", false}, {"http://example.com/mcp", false}, {"https://example.com/other", false}} {
		if Allows(key, tc.url) != tc.want {
			t.Fatal(tc)
		}
	}
	for _, raw := range []string{"http://example.com", "ftp://127.0.0.1", "https://user:secret@example.com"} {
		if _, err := Canonical(raw); err == nil {
			t.Fatal("accepted unsafe URL")
		}
	}
	if err = s.Set(key, nil); err != nil {
		t.Fatal(err)
	}
	e, err := s.Get(key)
	if err != nil || e.AccessToken != "" {
		t.Fatal("logout failed")
	}
}

func TestLoginDiscoveryPKCE(t *testing.T) {
	for _, header := range []bool{false, true} {
		for _, badState := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "fallback", true: "header"}[header], map[bool]string{false: "valid", true: "state-mismatch"}[badState]}, "/"), func(t *testing.T) {
				var base, challenge string
				var registrations, exchanges atomic.Int32
				mux := http.NewServeMux()
				srv := httptest.NewServer(mux)
				defer srv.Close()
				base = srv.URL
				mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
					if header {
						w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/metadata"`)
					}
					w.WriteHeader(401)
				})
				resource := func(w http.ResponseWriter, r *http.Request) {
					json.NewEncoder(w).Encode(map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base}})
				}
				mux.HandleFunc("/metadata", resource)
				mux.HandleFunc("/.well-known/oauth-protected-resource", resource)
				mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
					json.NewEncoder(w).Encode(map[string]string{"issuer": base, "authorization_endpoint": base + "/authorize?audience=example", "token_endpoint": base + "/token", "registration_endpoint": base + "/register"})
				})
				mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
					registrations.Add(1)
					json.NewEncoder(w).Encode(map[string]string{"client_id": "synthetic-client"})
				})
				mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
					r.ParseForm()
					sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
					if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || r.Form.Get("resource") != base+"/mcp" {
						t.Error("PKCE or resource mismatch")
					}
					if exchanges.Add(1) == 1 {
						json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-access", "refresh_token": "synthetic-refresh", "token_type": "Bearer", "expires_in": 3600})
					} else {
						json.NewEncoder(w).Encode(map[string]any{"access_token": "account-b", "token_type": "Bearer"})
					}
				})
				store := Store{Path: filepath.Join(t.TempDir(), "mcp-auth.json")}
				opener := func(raw string) error {
					u, _ := url.Parse(raw)
					q := u.Query()
					challenge = q.Get("code_challenge")
					if q.Get("audience") != "example" || q.Get("code_challenge_method") != "S256" || q.Get("resource") != base+"/mcp" {
						t.Error("authorization parameters")
					}
					callback, _ := url.Parse(q.Get("redirect_uri"))
					v := callback.Query()
					v.Set("code", "synthetic-code")
					if badState {
						callback.RawQuery = v.Encode()
						resp, err := http.Get(callback.String())
						if err != nil {
							return err
						}
						resp.Body.Close()
						if resp.StatusCode != http.StatusBadRequest {
							t.Error("missing state accepted")
						}
					}
					v.Set("state", q.Get("state"))
					if badState {
						v.Set("state", "wrong")
					}
					callback.RawQuery = v.Encode()
					resp, err := http.Get(callback.String())
					if err == nil {
						resp.Body.Close()
						if badState && resp.StatusCode != http.StatusBadRequest {
							t.Error("invalid state accepted")
						}
					}
					if err != nil {
						return err
					}
					if badState {
						v.Set("state", q.Get("state"))
						callback.RawQuery = v.Encode()
						resp, err = http.Get(callback.String())
						if err == nil {
							resp.Body.Close()
						}
					}
					return err
				}
				err := Login(context.Background(), store, base+"/mcp", io.Discard, opener)
				if err != nil {
					t.Fatal(err)
				}
				e, err := store.Get(base + "/mcp")
				if err != nil || e.AccessToken != "synthetic-access" || registrations.Load() != 1 {
					t.Fatal("missing credentials")
				}
				secondOpener := func(raw string) error {
					lock := keyLock(store.Path + base + "/mcp")
					select {
					case lock <- struct{}{}:
						lock.release()
					default:
						t.Error("login holds per-key lock during browser interaction")
					}
					// A successful explicit login recreates credentials after a concurrent logout.
					if err := store.Set(base+"/mcp", nil); err != nil {
						return err
					}
					return opener(raw)
				}
				if err := Login(context.Background(), store, base+"/mcp", io.Discard, secondOpener); err != nil {
					t.Fatal(err)
				}
				if registrations.Load() != 1 {
					t.Fatal("registration not reused")
				}
				e, err = store.Get(base + "/mcp")
				if err != nil || e.AccessToken != "account-b" || e.RefreshToken != "" || e.ClientID != "synthetic-client" {
					t.Fatal("explicit account switch did not win logout or retained old refresh token", err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				err = Login(ctx, store, base+"/mcp", io.Discard, func(string) error { cancel(); return nil })
				if !errors.Is(err, context.Canceled) {
					t.Fatal("callback cancellation lost cause", err)
				}
			})
		}
	}
}

func TestRefreshAndRetry(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "401", true: "expiry"}[expired], func(t *testing.T) {
			var calls, refreshes atomic.Int32
			mux := http.NewServeMux()
			srv := httptest.NewServer(mux)
			defer srv.Close()
			mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
				refreshes.Add(1)
				r.ParseForm()
				if r.Form.Get("resource") != srv.URL+"/mcp" {
					t.Error("resource missing")
				}
				json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "token_type": "Bearer", "expires_in": 3600})
			})
			mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer fresh" {
					w.WriteHeader(401)
				}
			})
			s := Store{Path: filepath.Join(t.TempDir(), "mcp-auth.json")}
			e := Entry{AccessToken: "old", RefreshToken: "refresh", Resource: srv.URL + "/mcp", TokenEndpoint: srv.URL + "/token"}
			if expired {
				e.Expiry = time.Now().Add(-time.Hour)
			}
			if err := s.Set(e.Resource, &e); err != nil {
				t.Fatal(err)
			}
			c := HTTPClient(nil, s, e.Resource, "test")
			resp, err := c.Get(e.Resource)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			want := int32(2)
			if expired {
				want = 1
			}
			if calls.Load() != want || refreshes.Load() != 1 {
				t.Fatal(calls.Load(), refreshes.Load())
			}
			req, _ := http.NewRequest("GET", e.Resource, nil)
			req.Header.Set("Authorization", "Bearer explicit")
			resp, err = c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if refreshes.Load() != 1 {
				t.Fatal("explicit auth refreshed")
			}
		})
	}
}

func TestDiscoveryRejectsMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"resource": "https://other.invalid/mcp", "authorization_servers": []string{"http://unsafe.invalid"}})
	}))
	defer srv.Close()
	if _, err := discover(context.Background(), Client(nil), srv.URL+"/mcp"); err == nil {
		t.Fatal("accepted resource mismatch")
	}
}

func TestCrossOriginAndRetryLimit(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(r.Header.Get("Authorization") != "") }))
	defer other.Close()
	var calls, refreshes atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) })
	mux.HandleFunc("/mcp/denied", func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(401) })
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "token_type": "Bearer"})
	})
	s := Store{Path: filepath.Join(t.TempDir(), "mcp-auth.json")}
	key := srv.URL + "/mcp"
	if err := s.Set(key, &Entry{AccessToken: "old", RefreshToken: "refresh", Resource: key, TokenEndpoint: srv.URL + "/token"}); err != nil {
		t.Fatal(err)
	}
	c := HTTPClient(nil, s, key, "test")
	resp, err := c.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if leaked.Load() {
		t.Fatal("token leaked")
	}
	resp, err = c.Get(other.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if leaked.Load() {
		t.Fatal("token leaked to direct request")
	}
	_, err = c.Get(key + "/denied")
	if err == nil || !strings.Contains(err.Error(), "zut mcp login test") || calls.Load() != 2 || refreshes.Load() != 1 {
		t.Fatal(err, calls.Load(), refreshes.Load())
	}
}
