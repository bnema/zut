package agent

import (
	"sort"

	"github.com/bnema/zut/packages/provider"
)

// modelPickerProviders returns providers whose models can be selected without
// prompting for credentials. Any custom provider with a configured endpoint
// (provider-level or per-model URL) is offered, even without a stored key,
// because such endpoints may be keyless; auth:"none" is not required.
func modelPickerProviders() []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range knownProviders {
		if CredentialAvailable(p) && !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	custom := provider.CustomProviders()
	names := make([]string, 0, len(custom))
	for p := range custom {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		if seen[p] {
			continue
		}
		cfg := custom[p]
		// Custom endpoints with a configured URL may run keyless, so picker
		// availability must not require stored credentials.
		keyless := !isBuiltinProvider(p) && (cfg.BaseURL != "" || customProviderHasModelURL(p))
		if keyless || CredentialAvailable(p) {
			out = append(out, p)
			seen[p] = true
		}
	}
	// Ollama models are always available (no auth needed).
	if !seen["ollama"] {
		out = append(out, "ollama")
	}
	return out
}

func customProviderHasModelURL(name string) bool {
	for _, m := range provider.ModelsForProvider(name) {
		if m.Source == "user" && m.BaseURL != "" {
			return true
		}
	}
	return false
}
