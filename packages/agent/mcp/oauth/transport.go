package oauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type transport struct {
	base      http.RoundTripper
	store     Store
	key, name string
}

// HTTPClient wraps a private copy of base, leaving explicitly configured auth alone.
func HTTPClient(base *http.Client, store Store, raw, name string) *http.Client {
	c := Client(base)
	key, err := Canonical(raw)
	if err != nil {
		return c
	}
	rt := c.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	c.Transport = &transport{base: rt, store: store, key: key, name: name}
	return c
}
func (t *transport) token(ctx context.Context, rejected string, force bool) (Entry, error) {
	lock := keyLock(t.store.Path + t.key)
	lock.Lock()
	defer lock.Unlock()
	e, err := t.store.Get(t.key)
	if err != nil {
		return e, err
	}
	if e.AccessToken == "" {
		return e, nil
	}
	if force && e.AccessToken != rejected {
		return e, nil
	}
	if !force && (e.Expiry.IsZero() || time.Now().Before(e.Expiry.Add(-60*time.Second))) {
		return e, nil
	}
	if e.RefreshToken == "" {
		return e, errors.New("OAuth refresh unavailable")
	}
	if err := exchange(ctx, Client(nil), &e, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {e.RefreshToken}}); err != nil {
		return Entry{}, err
	}
	return e, t.store.Set(t.key, &e)
}
func (t *transport) guidance() error {
	return fmt.Errorf("MCP authentication required; run zut mcp login %s", t.name)
}
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "" || !Allows(t.key, req.URL.String()) {
		return t.base.RoundTrip(req)
	}
	e, err := t.token(req.Context(), "", false)
	if err != nil {
		return nil, t.guidance()
	}
	send := func(e Entry, retry bool) (*http.Response, error) {
		clone := req.Clone(req.Context())
		if retry && req.Body != nil {
			if req.GetBody == nil {
				return nil, errors.New("MCP OAuth request cannot be replayed")
			}
			var err error
			clone.Body, err = req.GetBody()
			if err != nil {
				return nil, errors.New("MCP OAuth request replay failed")
			}
		}
		if e.AccessToken != "" {
			clone.Header.Set("Authorization", "Bearer "+e.AccessToken)
		}
		resp, err := t.base.RoundTrip(clone)
		if err != nil {
			return nil, errors.New("MCP HTTP request failed")
		}
		if e.AccessToken != "" && resp.StatusCode >= 300 {
			resp.Body.Close()
			resp.Body = io.NopCloser(strings.NewReader(""))
			resp.ContentLength = 0
		}
		return resp, nil
	}
	resp, err := send(e, false)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	resp.Body.Close()
	if e.AccessToken == "" {
		return nil, t.guidance()
	}
	refreshed, err := t.token(req.Context(), e.AccessToken, true)
	if err != nil || refreshed.AccessToken == "" {
		return nil, t.guidance()
	}
	resp, err = send(refreshed, true)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, t.guidance()
	}
	return resp, err
}
