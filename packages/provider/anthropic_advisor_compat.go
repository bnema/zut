package provider

import (
	"regexp"
	"slices"
)

// anthropicAdvisorPairs is Anthropic's documented executor -> allowed
// advisor table for the native advisor tool. Pairs outside it are rejected
// by the API with a 400, so unknown models are treated as incompatible and
// callers fall back to the client-side advisor.
var anthropicAdvisorPairs = map[string][]string{
	"claude-fable-5-2":  {"claude-mythos-5-2", "claude-fable-5-2"},
	"claude-mythos-5-2": {"claude-mythos-5-2", "claude-fable-5-2"},
	"claude-opus-5-5":   {"claude-mythos-5-2", "claude-fable-5-2", "claude-mythos-5-1", "claude-fable-5-1", "claude-mythos-5", "claude-fable-5", "claude-opus-5-5", "claude-opus-5"},
	"claude-sonnet-5-5": {"claude-mythos-5-2", "claude-fable-5-2", "claude-mythos-5-1", "claude-fable-5-1", "claude-mythos-5", "claude-fable-5", "claude-opus-5-5", "claude-opus-5", "claude-sonnet-5-5"},
	"claude-haiku-5-5":  {"claude-mythos-5-2", "claude-fable-5-2", "claude-mythos-5-1", "claude-fable-5-1", "claude-mythos-5", "claude-fable-5", "claude-opus-5-5", "claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "claude-sonnet-5-5", "claude-sonnet-5", "claude-haiku-5-5"},
	"claude-fable-5-1":  {"claude-mythos-5-2", "claude-fable-5-2", "claude-mythos-5-1", "claude-fable-5-1"},
	"claude-mythos-5-1": {"claude-mythos-5-2", "claude-fable-5-2", "claude-mythos-5-1", "claude-fable-5-1"},
	"claude-fable-5":    {"claude-mythos-5-2", "claude-fable-5-2", "claude-mythos-5-1", "claude-fable-5-1", "claude-mythos-5", "claude-fable-5", "claude-opus-5-5", "claude-opus-5"},
	"claude-mythos-5":   {"claude-mythos-5-2", "claude-fable-5-2", "claude-mythos-5-1", "claude-fable-5-1", "claude-mythos-5", "claude-fable-5", "claude-opus-5-5", "claude-opus-5"},
	"claude-opus-5":     {"claude-mythos-5-2", "claude-fable-5-2", "claude-mythos-5-1", "claude-fable-5-1", "claude-mythos-5", "claude-fable-5", "claude-opus-5-5", "claude-opus-5"},
	"claude-opus-4-8":   {"claude-mythos-5-2", "claude-fable-5-2", "claude-mythos-5-1", "claude-fable-5-1", "claude-mythos-5", "claude-fable-5", "claude-opus-5-5", "claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "claude-sonnet-5-5"},
	"claude-opus-4-7":   {"claude-mythos-5-2", "claude-fable-5-2", "claude-mythos-5-1", "claude-fable-5-1", "claude-mythos-5", "claude-fable-5", "claude-opus-5-5", "claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "claude-sonnet-5-5"},
	"claude-opus-4-6":   {"claude-mythos-5-2", "claude-fable-5-2", "claude-mythos-5-1", "claude-fable-5-1", "claude-mythos-5", "claude-fable-5", "claude-opus-5-5", "claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6", "claude-sonnet-5-5", "claude-sonnet-5", "claude-haiku-5-5"},
	"claude-sonnet-5":   {"claude-mythos-5-2", "claude-fable-5-2", "claude-mythos-5-1", "claude-fable-5-1", "claude-mythos-5", "claude-fable-5", "claude-opus-5-5", "claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "claude-sonnet-5-5", "claude-sonnet-5", "claude-haiku-5-5"},
	"claude-sonnet-4-6": {"claude-mythos-5-2", "claude-fable-5-2", "claude-mythos-5-1", "claude-fable-5-1", "claude-mythos-5", "claude-fable-5", "claude-opus-5-5", "claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6", "claude-sonnet-5-5", "claude-sonnet-5", "claude-sonnet-4-6", "claude-haiku-5-5"},
	"claude-haiku-4-5":  {"claude-mythos-5-2", "claude-fable-5-2", "claude-mythos-5-1", "claude-fable-5-1", "claude-mythos-5", "claude-fable-5", "claude-opus-5-5", "claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6", "claude-sonnet-5-5", "claude-sonnet-5", "claude-sonnet-4-6", "claude-haiku-5-5"},
}

// anthropicDatedSuffix matches a trailing snapshot date such as
// "-20250805"; dated IDs resolve to their alias.
var anthropicDatedSuffix = regexp.MustCompile(`-\d{8}$`)

func anthropicAdvisorAlias(id string) string {
	return anthropicDatedSuffix.ReplaceAllString(id, "")
}

// AnthropicAdvisorCompatible reports whether Anthropic's native advisor tool
// accepts advisorID for executorID, per the documented compatibility table.
func AnthropicAdvisorCompatible(executorID, advisorID string) bool {
	allowed, ok := anthropicAdvisorPairs[anthropicAdvisorAlias(executorID)]
	return ok && slices.Contains(allowed, anthropicAdvisorAlias(advisorID))
}
