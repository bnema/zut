// Package oauth owns credentials and OAuth login for HTTP MCP servers.
package oauth

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry binds a registration and token set to one canonical resource URL.
type Entry struct {
	ClientID      string    `json:"client_id"`
	ClientSecret  string    `json:"client_secret,omitempty"`
	RedirectURI   string    `json:"redirect_uri"`
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	Expiry        time.Time `json:"expiry,omitempty"`
	TokenType     string    `json:"token_type,omitempty"`
	Scope         string    `json:"scope,omitempty"`
	Issuer        string    `json:"issuer"`
	Resource      string    `json:"resource"`
	TokenEndpoint string    `json:"token_endpoint"`
}

// Canonical validates the transport and removes query and fragment components.
func Canonical(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return "", errors.New("invalid OAuth URL")
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")) {
		return "", errors.New("OAuth requires HTTPS (except loopback HTTP)")
	}
	u.Host = strings.ToLower(u.Host)
	u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

// Allows restricts bearer tokens to the resource's origin and path subtree.
func Allows(key, target string) bool {
	k, err := Canonical(key)
	if err != nil {
		return false
	}
	t, err := Canonical(target)
	if err != nil {
		return false
	}
	a, _ := url.Parse(k)
	b, _ := url.Parse(t)
	return a.Scheme == b.Scheme && a.Host == b.Host && (path.Clean(b.Path) == path.Clean(a.Path) || strings.HasPrefix(path.Clean(b.Path), strings.TrimRight(path.Clean(a.Path), "/")+"/"))
}

var fileMu sync.Mutex
var keys sync.Map

func keyLock(key string) *sync.Mutex {
	v, _ := keys.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// Store uses atomic, private files. Each update reloads existing entries.
type Store struct{ Path string }

func (s Store) load() (map[string]Entry, error) {
	m := map[string]Entry{}
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, errors.New("read MCP credentials failed")
	}
	if json.Unmarshal(b, &m) != nil {
		return nil, errors.New("invalid MCP credentials file")
	}
	if m == nil {
		m = map[string]Entry{}
	}
	return m, nil
}
func (s Store) Get(key string) (Entry, error) {
	fileMu.Lock()
	defer fileMu.Unlock()
	m, err := s.load()
	return m[key], err
}
func (s Store) Set(key string, e *Entry) error {
	fileMu.Lock()
	defer fileMu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	if e == nil {
		delete(m, key)
	} else {
		m[key] = *e
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		return errors.New("create MCP credential directory failed")
	}
	if err := os.Chmod(filepath.Dir(s.Path), 0700); err != nil {
		return errors.New("secure MCP credential directory failed")
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return errors.New("encode MCP credentials failed")
	}
	f, err := os.CreateTemp(filepath.Dir(s.Path), ".mcp-auth-*")
	if err != nil {
		return errors.New("create MCP credential file failed")
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), s.Path)
	}
	if err != nil {
		return errors.New("save MCP credentials failed")
	}
	return nil
}
