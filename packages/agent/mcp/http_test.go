package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func httpServerConfig(srv *httptest.Server) ServerConfig {
	return ServerConfig{Type: "http", URL: srv.URL + "/mcp"}
}

func startHTTP(t *testing.T, fake *httpFake, tweak func(*ServerConfig)) (*Manager, []error) {
	t.Helper()
	srv := fake.start(t)
	cfg := httpServerConfig(srv)
	if tweak != nil {
		tweak(&cfg)
	}
	m, failures, _ := startManager(t, map[string]ServerConfig{"web": cfg}, "")
	return m, failures
}

func TestManagerHTTPEndToEnd(t *testing.T) {
	for _, mode := range []struct {
		name string
		sse  bool
	}{{"json", false}, {"sse", true}} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("MCP_T_BEARER", "  s3cret-token \n")
			t.Setenv("MCP_T_ENVHDR", "from-env")
			fake := newHTTPFake(mode.sse)
			fake.wantAuth = "Bearer s3cret-token"
			m, failures := startHTTP(t, fake, func(c *ServerConfig) {
				c.BearerTokenEnvVar = "MCP_T_BEARER"
				c.Headers = map[string]string{"X-Static": "static"}
				c.EnvHeaders = map[string]string{"X-Env": "MCP_T_ENVHDR"}
			})
			if len(failures) != 0 {
				t.Fatalf("failures: %v", failures)
			}
			if st := m.Status()[0]; !st.Connected || st.Transport != "http" || st.Tools != fakeToolCount || !st.Resources {
				t.Fatalf("status = %+v", st)
			}

			// tool calls of each flavour
			if got := resultText(execTool(t, findTool(t, m, "mcp__web__echo"), `{"text":"hi"}`)); got != "echo: hi" {
				t.Errorf("echo = %q", got)
			}
			res := execTool(t, findTool(t, m, "mcp__web__image"), `{}`)
			if len(res.Content) != 2 {
				t.Errorf("image content = %+v", res.Content)
			}
			if res := execTool(t, findTool(t, m, "mcp__web__err"), `{}`); !res.IsError {
				t.Error("isError lost over http")
			}
			if got := resultText(execTool(t, findTool(t, m, "mcp__web__structured"), `{}`)); got != `{"answer":42}` {
				t.Errorf("structured = %q", got)
			}
			if got := resultText(execTool(t, findTool(t, m, ReadResourceToolName), `{"server":"web","uri":"mem://a"}`)); !strings.Contains(got, "content of mem://a") {
				t.Errorf("read resource = %q", got)
			}
			if got := resultText(execTool(t, findTool(t, m, ListResourcesToolName), `{}`)); !strings.Contains(got, "mem://a") || !strings.Contains(got, "template") {
				t.Errorf("list resources = %q", got)
			}

			// request headers
			recs := fake.snapshot()
			if len(recs) == 0 || recs[0].RPCMethod != "initialize" {
				t.Fatalf("first request = %+v", recs)
			}
			var sawInitialized, sawGet bool
			for i, r := range recs {
				if r.Auth != "Bearer s3cret-token" || r.Static != "static" || r.EnvHeader != "from-env" {
					t.Errorf("record %d missing configured headers: %+v", i, r)
				}
				if i == 0 {
					if r.Session != "" || r.Protocol != "" {
						t.Errorf("initialize must carry no session/protocol: %+v", r)
					}
					continue
				}
				if r.Session != "sess-abc" {
					t.Errorf("record %d (%s %s): session = %q", i, r.Method, r.RPCMethod, r.Session)
				}
				if r.Protocol != "2025-06-18" {
					t.Errorf("record %d (%s %s): protocol = %q", i, r.Method, r.RPCMethod, r.Protocol)
				}
				if r.RPCMethod == "notifications/initialized" {
					sawInitialized = true
				}
				if r.Method == http.MethodGet {
					sawGet = true
				}
			}
			if !sawInitialized {
				t.Error("notifications/initialized never sent")
			}
			_ = sawGet // the GET stream is opened asynchronously; checked separately below.

			// 405 on GET is tolerated: the optional server stream is skipped.
			eventually(t, "GET stream probe", func() bool {
				for _, r := range fake.snapshot() {
					if r.Method == http.MethodGet && r.Session == "sess-abc" && r.Protocol == "2025-06-18" {
						return true
					}
				}
				return false
			})

			// Close ends the session with DELETE.
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case sid := <-fake.deleted:
				if sid != "sess-abc" {
					t.Errorf("DELETE session = %q", sid)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no DELETE on Close")
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case sid := <-fake.deleted:
				t.Errorf("second Close sent another DELETE (%q)", sid)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

func TestManagerHTTPServerRequestDuringSSEResponse(t *testing.T) {
	fake := newHTTPFake(true)
	m, failures := startHTTP(t, fake, nil)
	if len(failures) != 0 {
		t.Fatal(failures)
	}
	res := execTool(t, findTool(t, m, "mcp__web__ask_ping"), `{}`)
	if res.IsError || resultText(res) != "ping reply: {}" {
		t.Fatalf("got %q (error=%v)", resultText(res), res.IsError)
	}
}

func TestManagerHTTPUnauthorized(t *testing.T) {
	t.Run("wrong token", func(t *testing.T) {
		t.Setenv("MCP_T_BEARER", "wrong")
		fake := newHTTPFake(false)
		fake.wantAuth = "Bearer right"
		_, failures := startHTTP(t, fake, func(c *ServerConfig) { c.BearerTokenEnvVar = "MCP_T_BEARER" })
		if len(failures) != 1 {
			t.Fatalf("failures = %v", failures)
		}
		msg := failures[0].Error()
		for _, want := range []string{"401", "Unauthorized", "bearerTokenEnvVar", `"web"`} {
			if !strings.Contains(msg, want) {
				t.Errorf("message missing %q: %s", want, msg)
			}
		}
		if strings.Contains(msg, "Bearer wrong") {
			t.Errorf("message leaks the token: %s", msg)
		}
	})
	t.Run("no credentials", func(t *testing.T) {
		fake := newHTTPFake(true)
		fake.wantAuth = "Bearer right"
		_, failures := startHTTP(t, fake, nil)
		if len(failures) != 1 || !strings.Contains(failures[0].Error(), "401") {
			t.Fatalf("failures = %v", failures)
		}
	})
	t.Run("missing bearer variable", func(t *testing.T) {
		os.Unsetenv("MCP_T_NOPE")
		fake := newHTTPFake(false)
		_, failures := startHTTP(t, fake, func(c *ServerConfig) { c.BearerTokenEnvVar = "MCP_T_NOPE" })
		if len(failures) != 1 || !strings.Contains(failures[0].Error(), "MCP_T_NOPE is not set") {
			t.Fatalf("failures = %v", failures)
		}
		if n := len(fake.snapshot()); n != 0 {
			t.Errorf("no request should be sent without a token, got %d", n)
		}
	})
}

func TestHTTPStatusErrorMessage(t *testing.T) {
	err := httpStatusError(http.StatusForbidden, []byte("  nope  "))
	got := err.Error()
	if !strings.Contains(got, "403") || !strings.Contains(got, "bearerTokenEnvVar") || !strings.HasSuffix(got, ": nope") {
		t.Errorf("got %q", got)
	}
	long := httpStatusError(http.StatusInternalServerError, []byte(strings.Repeat("x", 1000))).Error()
	if len(long) > 400 || strings.Contains(long, "bearerTokenEnvVar") {
		t.Errorf("long body not trimmed or hint misapplied: %d", len(long))
	}
}

func TestManagerHTTPToolTimeoutSendsCancel(t *testing.T) {
	fake := newHTTPFake(false)
	m, failures := startHTTP(t, fake, func(c *ServerConfig) { c.ToolTimeoutSec = 0.3 })
	if len(failures) != 0 {
		t.Fatal(failures)
	}
	res := execTool(t, findTool(t, m, "mcp__web__slow"), `{}`)
	if !res.IsError || !strings.Contains(resultText(res), "toolTimeoutSec") {
		t.Fatalf("got %q", resultText(res))
	}
	eventually(t, "cancelled notification", func() bool {
		for _, r := range fake.snapshot() {
			if r.RPCMethod == "notifications/cancelled" {
				return true
			}
		}
		return false
	})
	if got := resultText(execTool(t, findTool(t, m, "mcp__web__echo"), `{"text":"ok"}`)); got != "echo: ok" {
		t.Errorf("after timeout = %q", got)
	}
}

func TestManagerHTTPStartupFailureUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens any more
	m, failures, reqErr := startManager(t, map[string]ServerConfig{"gone": {Type: "http", URL: url, Required: true}}, "")
	if len(failures) != 1 || reqErr == nil {
		t.Fatalf("failures=%v reqErr=%v", failures, reqErr)
	}
	if st := m.Status()[0]; st.Connected || st.Err == "" {
		t.Errorf("status = %+v", st)
	}
}

func TestHTTPTransportPlainResponses(t *testing.T) {
	// 202 for notifications and a JSON batch answer are accepted.
	var gotNotify bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg message
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if !msg.isRequest() {
			gotNotify = true
			w.WriteHeader(http.StatusAccepted)
			return
		}
		note, _ := encodeNotification("notifications/message", nil)
		resp, _ := encodeResponse(msg.ID, map[string]any{"ok": true}, nil)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte("[" + string(note) + "," + string(resp) + "]"))
	}))
	defer srv.Close()
	tr, err := newHTTPTransport("x", ServerConfig{URL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tr.notify(ctx, "notifications/initialized", nil); err != nil || !gotNotify {
		t.Fatalf("notify: %v (server saw=%v)", err, gotNotify)
	}
	raw, err := tr.request(ctx, 5, "anything", nil)
	if err != nil || string(raw) != `{"ok":true}` {
		t.Fatalf("batch response: %s %v", raw, err)
	}
	// After close, requests fail fast.
	_ = tr.close()
	if _, err := tr.request(ctx, 6, "anything", nil); err == nil {
		t.Error("request after close should fail")
	}
}

func TestReadSSEFrames(t *testing.T) {
	stream := ": comment\n" +
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"a\"}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\n" +
		"data: \"id\":3,\"result\":{}}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"method\":\"never\"}\n\n"
	var methods []string
	err := readSSEFrames(strings.NewReader(stream), func(m message) bool {
		methods = append(methods, m.Method)
		return m.isResponse()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(methods) != 2 || methods[0] != "a" || methods[1] != "" {
		t.Errorf("methods = %q (multi-line data must join and stop after the response)", methods)
	}
	// Trailing event without a blank line is still delivered.
	n := 0
	_ = readSSEFrames(strings.NewReader("data: {\"jsonrpc\":\"2.0\",\"method\":\"z\"}"), func(message) bool { n++; return false })
	if n != 1 {
		t.Errorf("unterminated event delivered %d times", n)
	}
}
