package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransportSettings(t *testing.T) {
	tr := NewTransport(TransportOptions{})
	defer tr.CloseIdleConnections()
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout != 15*time.Second || tr.HTTP2.PingTimeout != 10*time.Second {
		t.Fatalf("unexpected HTTP/2 settings: %+v", tr.HTTP2)
	}
	if tr.Proxy == nil || tr.DialContext == nil || !tr.ForceAttemptHTTP2 || tr.MaxIdleConns != 100 || tr.IdleConnTimeout != 90*time.Second || tr.TLSHandshakeTimeout != 10*time.Second || tr.ExpectContinueTimeout != time.Second || tr.ResponseHeaderTimeout != 0 {
		t.Fatalf("unexpected transport settings: %+v", tr)
	}
	if tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("secure transport skips verification")
	}
	insecure := NewTransport(TransportOptions{InsecureTLS: true})
	defer insecure.CloseIdleConnections()
	if insecure.TLSClientConfig == nil || !insecure.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("insecure option ignored")
	}
	if sharedTransport() != sharedTransport() || newDefaultHTTPClient().Transport != sharedTransport() || NewHTTPClient(false).Transport != sharedTransport() {
		t.Fatal("default clients do not share a pool")
	}
	if NewHTTPClient(true).Transport == sharedTransport() {
		t.Fatal("insecure client shares secure pool")
	}
	if newDefaultHTTPClient().Timeout != 0 {
		t.Fatal("default client limits streaming duration")
	}
}

// blackholeProxy forwards TLS bytes unchanged. Only the first connection can
// become a blackhole; subsequent connections continue to reach the server.
func blackholeProxy(t *testing.T, target string) (string, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var swallow atomic.Bool
	var accepted atomic.Int32
	var workers sync.WaitGroup
	var mu sync.Mutex
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			downstream, err := listener.Accept()
			if err != nil {
				return
			}
			first := accepted.Add(1) == 1
			upstream, err := net.DialTimeout("tcp", target, time.Second)
			if err != nil {
				downstream.Close()
				return
			}
			mu.Lock()
			conns = append(conns, downstream, upstream)
			mu.Unlock()
			pump := func(dst, src net.Conn) {
				defer workers.Done()
				buf := make([]byte, 32*1024)
				for {
					n, err := src.Read(buf)
					if n > 0 && !(first && swallow.Load()) {
						if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}
			workers.Add(2)
			go pump(upstream, downstream)
			go pump(downstream, upstream)
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
		mu.Lock()
		for _, conn := range conns {
			conn.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	return listener.Addr().String(), &swallow, &accepted
}

func TestTransportHTTP2Blackhole(t *testing.T) {
	for _, healthChecks := range []bool{true, false} {
		name := "pings"
		if !healthChecks {
			name = "no_pings_control"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			address, swallow, accepted := blackholeProxy(t, server.Listener.Addr().String())
			tr := newTransport(TransportOptions{InsecureTLS: true}, transportTimings{50 * time.Millisecond, 100 * time.Millisecond})
			if !healthChecks {
				tr.HTTP2 = nil
			}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr}
			url := "https://" + address
			requestOK := func() {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil || string(body) != "ok" || resp.ProtoMajor != 2 {
					t.Fatalf("response: proto=%s body=%q err=%v", resp.Proto, body, err)
				}
			}
			requestOK()
			swallow.Store(true)
			deadline := 5 * time.Second
			if !healthChecks {
				deadline = 600 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			start := time.Now()
			resp, err := client.Do(req)
			elapsed := time.Since(start)
			if resp != nil {
				resp.Body.Close()
			}
			if !healthChecks {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("without pings expected deadline, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "http2: client connection lost") || ctx.Err() != nil || elapsed >= 2*time.Second {
				t.Fatalf("expected prompt ping failure, got %v after %v (context: %v)", err, elapsed, ctx.Err())
			}
			requestOK()
			if accepted.Load() != 2 {
				t.Fatalf("expected fresh connection, accepted %d", accepted.Load())
			}
		})
	}
}
