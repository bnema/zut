package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
)

// Error categories returned by ErrorCategory. They are fixed labels safe to
// log: they never carry request or response content.
const (
	ErrorCategoryNetwork             = "network"
	ErrorCategoryTimeout             = "timeout"
	ErrorCategoryAuth                = "auth"
	ErrorCategoryPermission          = "permission"
	ErrorCategoryRateLimit           = "rate_limit"
	ErrorCategoryProviderUnavailable = "provider_unavailable"
	ErrorCategoryContextOverflow     = "context_overflow"
	ErrorCategoryOther               = "other"
)

// ErrorCategory maps a provider request error to a content-free category.
// Typed errors are checked first; the message is consulted only for HTTP
// status tokens ("http 429") and a few unambiguous provider phrases.
func ErrorCategory(err error) string {
	if err == nil {
		return ""
	}
	if IsContextOverflowError(err) {
		return ErrorCategoryContextOverflow
	}
	if status, ok := typedHTTPStatus(err); ok {
		if c := statusCategory(status); c != "" {
			return c
		}
		return ErrorCategoryOther
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorCategoryTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return ErrorCategoryTimeout
		}
		return ErrorCategoryNetwork
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrorCategoryNetwork
	}
	msg := strings.ToLower(err.Error())
	for _, code := range []int{401, 403, 429, 500, 502, 503, 504} {
		if hasHTTPStatus(msg, strconv.Itoa(code)) {
			return statusCategory(code)
		}
	}
	for _, p := range []struct{ category, phrase string }{
		{ErrorCategoryAuth, "invalid_authentication"},
		{ErrorCategoryAuth, "api key appears to be invalid"},
		{ErrorCategoryRateLimit, "too many requests"},
		{ErrorCategoryRateLimit, "rate_limit_exceeded"},
		{ErrorCategoryProviderUnavailable, "service unavailable"},
		{ErrorCategoryProviderUnavailable, "upstream connect error"},
	} {
		if strings.Contains(msg, p.phrase) {
			return p.category
		}
	}
	return ErrorCategoryOther
}

func typedHTTPStatus(err error) (int, bool) {
	var transcribe *TranscribeHTTPError
	if errors.As(err, &transcribe) {
		return transcribe.StatusCode, true
	}
	var transient *transientHTTPError
	if errors.As(err, &transient) {
		return transient.Status, true
	}
	return 0, false
}

func statusCategory(status int) string {
	switch {
	case status == 401:
		return ErrorCategoryAuth
	case status == 403:
		return ErrorCategoryPermission
	case status == 429:
		return ErrorCategoryRateLimit
	case status >= 500 && status <= 599:
		return ErrorCategoryProviderUnavailable
	}
	return ""
}
