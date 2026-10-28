// Package oauth owns credentials and OAuth login for HTTP MCP servers.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
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

var keys sync.Map

type semaphore chan struct{}

func keyLock(key string) semaphore {
	v, _ := keys.LoadOrStore(key, make(semaphore, 1))
	return v.(semaphore)
}
func (s semaphore) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s semaphore) release() { <-s }

// Store uses atomic, private files. Each update reloads existing entries.
type Store struct{ Path string }

func (s Store) load() (map[string]Entry, error) {
	m := map[string]Entry{}
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, safeError("read MCP credentials failed", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, safeError("invalid MCP credentials file", err)
	}
	if m == nil {
		m = map[string]Entry{}
	}
	return m, nil
}
func (s Store) Get(key string) (Entry, error) { return s.get(context.Background(), key) }
func (s Store) get(ctx context.Context, key string) (Entry, error) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return Entry{}, err
	}
	defer unlock()
	m, err := s.load()
	return m[key], err
}
func (s Store) Set(ctx context.Context, key string, e *Entry) error {
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return s.set(key, e)
}

// set requires the store file lock and reloads unrelated entries before writing.
func (s Store) set(key string, e *Entry) error {
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
		return safeError("create MCP credential directory failed", err)
	}
	if err := os.Chmod(filepath.Dir(s.Path), 0700); err != nil {
		return safeError("secure MCP credential directory failed", err)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return safeError("encode MCP credentials failed", err)
	}
	f, err := os.CreateTemp(filepath.Dir(s.Path), ".mcp-auth-*")
	if err != nil {
		return safeError("create MCP credential file failed", err)
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
		return safeError("save MCP credentials failed", err)
	}
	return nil
}

// lock uses a stable sidecar inode: the credential file itself is atomically replaced.
func (s Store) lock(ctx context.Context) (func(), error) {
	// Bound callers without a deadline too, including standalone logout.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		return nil, safeError("create MCP credential directory failed", err)
	}
	f, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, safeError("open MCP credential lock failed", err)
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return nil, safeError("secure MCP credential lock failed", err)
	}
	for {
		if err = ctx.Err(); err != nil {
			f.Close()
			return nil, fmt.Errorf("mcp credentials locked by another zut process: %w", err)
		}
		acquired, lockErr := tryFileLock(f)
		if lockErr != nil {
			f.Close()
			return nil, safeError("lock MCP credentials failed", lockErr)
		}
		if acquired {
			return func() { unlockFile(f); f.Close() }, nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, fmt.Errorf("mcp credentials locked by another zut process: %w", ctx.Err())
		case <-timer.C:
		}
	}
}
