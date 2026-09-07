package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// DiscoveryFailure classifies catalog failures without retaining URLs, response
// bodies, credentials, or transport error strings.
type DiscoveryFailure uint8

const (
	DiscoveryNetwork DiscoveryFailure = iota
	DiscoveryDNS
	DiscoveryTimeout
	DiscoveryCanceled
	DiscoveryHTTP
	DiscoveryInvalidResponse
)

// DiscoveryError is safe to display and inspect with errors.As.
type DiscoveryError struct {
	Kind       DiscoveryFailure
	StatusCode int
}

func (e *DiscoveryError) Error() string {
	switch e.Kind {
	case DiscoveryDNS:
		return "DNS resolution failed; check the connection and retry with zut --list-models"
	case DiscoveryTimeout:
		return "request timed out; retry with zut --list-models"
	case DiscoveryCanceled:
		return "request canceled"
	case DiscoveryHTTP:
		switch e.StatusCode {
		case 401:
			return "authentication rejected (HTTP 401); check the API key or use /login"
		case 403:
			return "access denied (HTTP 403); check provider permissions or account policy"
		case 429:
			return "rate limited (HTTP 429); retry later"
		default:
			return fmt.Sprintf("provider returned HTTP %d; retry with zut --list-models", e.StatusCode)
		}
	case DiscoveryInvalidResponse:
		return "invalid catalog response; retry with zut --list-models"
	default:
		return "network request failed; check the connection and retry with zut --list-models"
	}
}

// ClassifyDiscoveryError strips potentially sensitive details at the transport
// boundary. Unknown errors are not interpolated into user-facing diagnostics.
func ClassifyDiscoveryError(err error) *DiscoveryError {
	var discovery *DiscoveryError
	if errors.As(err, &discovery) {
		copy := *discovery
		return &copy
	}
	if errors.Is(err, context.Canceled) {
		return &DiscoveryError{Kind: DiscoveryCanceled}
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return &DiscoveryError{Kind: DiscoveryDNS}
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &network) && network.Timeout()) {
		return &DiscoveryError{Kind: DiscoveryTimeout}
	}
	return &DiscoveryError{Kind: DiscoveryNetwork}
}
