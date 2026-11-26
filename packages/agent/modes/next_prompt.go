package modes

// nextPromptSuggestionsEnabled reports whether next prompt suggestions are
// on. nil means enabled.
func nextPromptSuggestionsEnabled(enabled *bool) bool {
	return enabled == nil || *enabled
}

// clearNextPromptSuggestion removes any shown next prompt ghost text.
//
// Locking: i.ed is owned by the Run loop and is not guarded by i.mu. Call
// this only from the Run loop (key handling, settings toggles, loopCalls);
// other goroutines must post it through i.loopCalls.
func (i *Interactive) clearNextPromptSuggestion() {
	i.ed.SetGhost("")
}
