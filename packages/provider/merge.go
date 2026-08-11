package provider

// MergeCatalog returns the baked-in catalog overlaid with live entries.
// Precedence per id: live > catalog; speculative entries are promoted
// to non-speculative when a matching live id appears.
//
// Unknown live ids (not in the static catalog) are appended at the end
// with placeholder prices so they still render in the picker. Prices
// can be populated later from a richer catalog source.
func MergeCatalog(live []Model) []Model {
	return MergeCatalogForProviders(live, nil)
}

// MergeCatalogForProviders merges live entries and omits baked-in models for
// providers whose live catalog is authoritative. This is needed for
// account-scoped catalogs, where a static fallback could otherwise expose a
// model the signed-in account cannot use.
func MergeCatalogForProviders(live []Model, authoritativeProviders []string) []Model {
	byKey := func(p, id string) string { return p + "/" + id }
	authoritative := make(map[string]bool, len(authoritativeProviders))
	for _, name := range authoritativeProviders {
		authoritative[name] = true
	}

	staticIndex := make(map[string]Model, len(Catalog))
	staticOrder := make([]string, 0, len(Catalog))
	for _, m := range Catalog {
		m.Source = "catalog"
		k := byKey(m.Provider, m.ID)
		staticIndex[k] = m
		if !authoritative[m.Provider] {
			staticOrder = append(staticOrder, k)
		}
	}

	// Promote/overwrite from live.
	for _, l := range live {
		k := byKey(l.Provider, l.ID)
		if s, ok := staticIndex[k]; ok {
			// Preserve static pricing and output limits while allowing live
			// discovery to supply the account's current display and context
			// metadata.
			s.Source = "live"
			s.Speculative = false
			if l.DisplayName != "" {
				s.DisplayName = l.DisplayName
			}
			if l.ContextWindow > 0 {
				s.ContextWindow = l.ContextWindow
			}
			staticIndex[k] = s
			if authoritative[l.Provider] {
				staticOrder = append(staticOrder, k)
			}
		} else {
			// New live id we'd never heard of. Best-effort defaults.
			if l.DisplayName == "" {
				l.DisplayName = l.ID
			}
			staticIndex[k] = l
			staticOrder = append(staticOrder, k)
		}
	}

	out := make([]Model, 0, len(staticOrder))
	for _, k := range staticOrder {
		out = append(out, staticIndex[k])
	}
	return out
}
