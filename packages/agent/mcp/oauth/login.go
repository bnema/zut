package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type metadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	Scopes                []string `json:"-"`
}

// Client never forwards credentials across redirects or prints server bodies.
func Client(base *http.Client) *http.Client {
	c := http.Client{Timeout: 30 * time.Second}
	if base != nil {
		c = *base
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many OAuth redirects")
		}
		if _, err := Canonical(req.URL.String()); err != nil {
			return err
		}
		if len(via) > 0 && (req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme) {
			req.Header.Del("Authorization")
			return http.ErrUseLastResponse
		}
		return nil
	}
	return &c
}
func jsonRequest(ctx context.Context, c *http.Client, method, endpoint, contentType string, body io.Reader, dst any) error {
	if _, err := Canonical(endpoint); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return errors.New("invalid OAuth endpoint")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.Do(req)
	if err != nil {
		return errors.New("OAuth endpoint request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("OAuth endpoint returned HTTP %d", resp.StatusCode)
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dst) != nil {
		return errors.New("invalid OAuth endpoint response")
	}
	return nil
}
func discover(ctx context.Context, c *http.Client, key string) (metadata, error) {
	var m metadata
	u, _ := url.Parse(key)
	endpoint := u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource"
	req, _ := http.NewRequestWithContext(ctx, "GET", key, nil)
	resp, err := c.Do(req)
	if err != nil {
		return m, errors.New("MCP OAuth discovery probe failed")
	}
	resp.Body.Close()
	for _, challenge := range resp.Header.Values("WWW-Authenticate") {
		// Read the resource_metadata parameter without including the challenge in errors.
		for _, part := range strings.Split(challenge, ",") {
			if i := strings.Index(strings.ToLower(part), "resource_metadata="); i >= 0 {
				endpoint = strings.Trim(strings.TrimSpace(part[i+len("resource_metadata="):]), "\"")
			}
		}
	}
	var resource struct {
		Resource string   `json:"resource"`
		Servers  []string `json:"authorization_servers"`
		Scopes   []string `json:"scopes_supported"`
	}
	if err := jsonRequest(ctx, c, "GET", endpoint, "", nil, &resource); err != nil {
		return m, err
	}
	canonical, err := Canonical(resource.Resource)
	if err != nil || canonical != key {
		return m, errors.New("OAuth protected resource does not match MCP server URL")
	}
	if len(resource.Servers) == 0 {
		return m, errors.New("OAuth resource has no authorization server")
	}
	issuer, err := Canonical(resource.Servers[0])
	if err != nil {
		return m, err
	}
	as, _ := url.Parse(issuer)
	path := strings.TrimRight(as.Path, "/")
	endpoints := []string{as.Scheme + "://" + as.Host + "/.well-known/oauth-authorization-server" + path, strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"}
	for _, endpoint := range endpoints {
		if err = jsonRequest(ctx, c, "GET", endpoint, "", nil, &m); err == nil {
			break
		}
	}
	if err != nil {
		return m, errors.New("OAuth authorization server discovery failed")
	}
	got, err := Canonical(m.Issuer)
	if err != nil || got != issuer {
		return m, errors.New("OAuth issuer mismatch")
	}
	for _, endpoint := range []string{m.AuthorizationEndpoint, m.TokenEndpoint} {
		if _, err := Canonical(endpoint); err != nil {
			return m, err
		}
	}
	if m.RegistrationEndpoint != "" {
		if _, err := Canonical(m.RegistrationEndpoint); err != nil {
			return m, err
		}
	}
	m.Scopes = resource.Scopes
	return m, nil
}
func random() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("OAuth randomness unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// OpenBrowser starts the platform browser without passing through a shell.
func OpenBrowser(raw string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", raw)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", raw)
	default:
		cmd = exec.Command("xdg-open", raw)
	}
	if err := cmd.Start(); err != nil {
		return errors.New("browser unavailable")
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// Login runs an authorization-code flow. The opener is injectable for local tests.
func Login(ctx context.Context, store Store, raw string, stderr io.Writer, opener func(string) error) error {
	key, err := Canonical(raw)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	c := Client(nil)
	m, err := discover(ctx, c, key)
	if err != nil {
		return err
	}
	lock := keyLock(store.Path + key)
	lock.Lock()
	defer lock.Unlock()
	e, err := store.Get(key)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return errors.New("OAuth callback listener failed")
	}
	defer listener.Close()
	redirect := "http://" + listener.Addr().String() + "/callback"
	// A reused registration must retain its registered callback port.
	if e.ClientID != "" && e.RedirectURI != "" {
		listener.Close()
		callback, parseErr := url.Parse(e.RedirectURI)
		if parseErr != nil || callback.Hostname() != "127.0.0.1" || callback.Scheme != "http" {
			return errors.New("invalid stored OAuth callback")
		}
		listener, err = net.Listen("tcp4", callback.Host)
		if err != nil {
			return errors.New("registered OAuth callback port unavailable")
		}
		defer listener.Close()
		redirect = e.RedirectURI
	}
	if e.ClientID == "" {
		if m.RegistrationEndpoint == "" {
			return errors.New("OAuth server has no registration endpoint or stored client registration")
		}
		payload, _ := json.Marshal(map[string]any{"client_name": "zut", "redirect_uris": []string{redirect}, "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "token_endpoint_auth_method": "none"})
		var registration struct {
			ID     string `json:"client_id"`
			Secret string `json:"client_secret"`
		}
		if err := jsonRequest(ctx, c, "POST", m.RegistrationEndpoint, "application/json", bytes.NewReader(payload), &registration); err != nil {
			return err
		}
		if registration.ID == "" {
			return errors.New("OAuth registration returned no client ID")
		}
		e.ClientID, e.ClientSecret = registration.ID, registration.Secret
	}
	if e.Issuer != "" && e.Issuer != m.Issuer {
		return errors.New("OAuth authorization server changed; log out before logging in again")
	}
	e.RedirectURI, e.Issuer, e.Resource, e.TokenEndpoint = redirect, m.Issuer, key, m.TokenEndpoint
	if e.Scope == "" {
		e.Scope = strings.Join(m.Scopes, " ")
	}
	state, err := random()
	if err != nil {
		return err
	}
	verifier, err := random()
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(verifier))
	type callbackResult struct {
		code string
		err  error
	}
	done := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		result := callbackResult{}
		if r.URL.Query().Get("state") != state {
			result.err = errors.New("OAuth state mismatch")
		} else if r.URL.Query().Get("error") != "" {
			result.err = errors.New("OAuth authorization denied")
		} else {
			result.code = r.URL.Query().Get("code")
			if result.code == "" {
				result.err = errors.New("OAuth callback has no code")
			}
		}
		select {
		case done <- result:
		default:
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintln(w, "Return to zut to continue.")
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	authorize, _ := url.Parse(m.AuthorizationEndpoint)
	// Do not echo arbitrary metadata query parameters (which may contain secrets).
	authorize.RawQuery, authorize.Fragment = "", ""
	q := url.Values{}
	for k, v := range map[string]string{"response_type": "code", "client_id": e.ClientID, "redirect_uri": redirect, "state": state, "code_challenge": base64.RawURLEncoding.EncodeToString(sum[:]), "code_challenge_method": "S256", "resource": key} {
		q.Set(k, v)
	}
	if e.Scope != "" {
		q.Set("scope", e.Scope)
	}
	authorize.RawQuery = q.Encode()
	fmt.Fprintln(stderr, "Open this URL to sign in:", authorize.String())
	if opener == nil {
		opener = OpenBrowser
	}
	_ = opener(authorize.String())
	var result callbackResult
	select {
	case <-ctx.Done():
		return errors.New("OAuth login cancelled or timed out")
	case result = <-done:
	}
	if result.err != nil {
		return result.err
	}
	values := url.Values{"grant_type": {"authorization_code"}, "code": {result.code}, "redirect_uri": {redirect}, "code_verifier": {verifier}}
	if err := exchange(ctx, c, &e, values); err != nil {
		return err
	}
	return store.Set(key, &e)
}
func exchange(ctx context.Context, c *http.Client, e *Entry, values url.Values) error {
	values.Set("client_id", e.ClientID)
	values.Set("resource", e.Resource)
	if e.ClientSecret != "" {
		values.Set("client_secret", e.ClientSecret)
	}
	var token struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Type    string `json:"token_type"`
		Scope   string `json:"scope"`
		Expires int64  `json:"expires_in"`
	}
	if err := jsonRequest(ctx, c, "POST", e.TokenEndpoint, "application/x-www-form-urlencoded", strings.NewReader(values.Encode()), &token); err != nil {
		return err
	}
	if token.Access == "" || !strings.EqualFold(token.Type, "Bearer") {
		return errors.New("OAuth server returned an invalid bearer token")
	}
	e.AccessToken, e.TokenType = token.Access, token.Type
	if token.Refresh != "" {
		e.RefreshToken = token.Refresh
	}
	if token.Scope != "" {
		e.Scope = token.Scope
	}
	e.Expiry = time.Time{}
	if token.Expires > 0 {
		e.Expiry = time.Now().Add(time.Duration(token.Expires) * time.Second)
	}
	return nil
}
