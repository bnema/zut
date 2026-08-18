package subagents

// Windows does not expose a no-follow regular-file read through os. Disable the
// no-rewrite fast path so atomic replacement remains the only projection path.
func residentProjectionCurrent(string, []byte) bool {
	return false
}
