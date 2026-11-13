package provider

import (
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	// transportDialTimeout bounds connection establishment.
	transportDialTimeout = 30 * time.Second
	// transportKeepAliveIdle starts TCP probes after inactivity.
	transportKeepAliveIdle = 15 * time.Second
	// transportKeepAliveInterval spaces TCP probes.
	transportKeepAliveInterval = 15 * time.Second
	// transportKeepAliveCount limits unanswered TCP probes.
	transportKeepAliveCount = 3
	// transportIdleTimeout retires unused pooled connections.
	transportIdleTimeout = 90 * time.Second
	// transportTLSHandshakeTimeout bounds TLS negotiation.
	transportTLSHandshakeTimeout = 10 * time.Second
	// transportExpectContinueTimeout bounds waiting for 100 Continue.
	transportExpectContinueTimeout = time.Second
	// transportSendPingTimeout probes silent HTTP/2 connections.
	transportSendPingTimeout = 15 * time.Second
	// transportPingTimeout bounds waiting for an HTTP/2 ping response.
	transportPingTimeout = 10 * time.Second
)

// TransportOptions configures a provider transport.
type TransportOptions struct {
	InsecureTLS bool
}

type transportTimings struct {
	sendPingTimeout time.Duration
	pingTimeout     time.Duration
}

// NewTransport builds a provider transport with HTTP/2 health checks.
func NewTransport(opts TransportOptions) *http.Transport {
	return newTransport(opts, transportTimings{transportSendPingTimeout, transportPingTimeout})
}

func newTransport(opts TransportOptions, timings transportTimings) *http.Transport {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:         transportDialTimeout,
			KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: transportKeepAliveIdle, Interval: transportKeepAliveInterval, Count: transportKeepAliveCount},
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       transportIdleTimeout,
		TLSHandshakeTimeout:   transportTLSHandshakeTimeout,
		ExpectContinueTimeout: transportExpectContinueTimeout,
		HTTP2:                 &http.HTTP2Config{SendPingTimeout: timings.sendPingTimeout, PingTimeout: timings.pingTimeout},
	}
	if opts.InsecureTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return tr
}

var defaultTransport = sync.OnceValue(func() *http.Transport {
	return NewTransport(TransportOptions{})
})

func sharedTransport() *http.Transport { return defaultTransport() }

func newDefaultHTTPClient() *http.Client {
	return &http.Client{Transport: sharedTransport()}
}
