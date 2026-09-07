package provider

import (
	"net/http"
	"strings"
)

const (
	openCodeGoClient    = "zut"
	openCodeGoUserAgent = "zut"
)

// setOpenCodeGoHeaders adds the identity OpenCode Go asks third-party agents
// to send. The values come from one logical Request and are rebuilt for every
// transport attempt, so retries preserve identity without sharing mutable
// header state between clients or runtimes.
func setOpenCodeGoHeaders(headers http.Header, providerName string, requestContext RequestContext) {
	if providerName != ProviderOpenCodeGo {
		return
	}

	session := strings.TrimSpace(requestContext.ThreadID)
	if session == "" {
		session = strings.TrimSpace(requestContext.CacheSessionID)
	}
	if session != "" {
		headers.Set("x-opencode-session", session)
	}
	if requestID := strings.TrimSpace(requestContext.TurnID); requestID != "" {
		headers.Set("x-opencode-request", requestID)
	}
	headers.Set("x-opencode-client", openCodeGoClient)
	headers.Set("user-agent", openCodeGoUserAgent)
}
