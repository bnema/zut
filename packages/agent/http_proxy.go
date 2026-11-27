package agent

import (
	"os"
	"strings"

	"github.com/bnema/zut/packages/agent/config"
)

// applyConfiguredHTTPProxy makes the persisted proxy available to Go's
// standard HTTP transport and to child processes. Explicit environment
// variables take precedence over config.json.
func applyConfiguredHTTPProxy() {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	proxy := strings.TrimSpace(cfg.HTTPProxy)
	if proxy == "" {
		return
	}
	if os.Getenv("HTTP_PROXY") == "" && os.Getenv("http_proxy") == "" {
		_ = os.Setenv("HTTP_PROXY", proxy)
	}
	if os.Getenv("HTTPS_PROXY") == "" && os.Getenv("https_proxy") == "" {
		_ = os.Setenv("HTTPS_PROXY", proxy)
	}
}
