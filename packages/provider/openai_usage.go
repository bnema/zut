package provider

// openAIInputTokensDetails is shared by the Chat Completions and Responses
// streams. The details object is optional; its presence is the availability
// signal for cache accounting, including a known cache miss.
type openAIInputTokensDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// normalizeOpenAIUsage converts OpenAI's total input count and optional cache
// detail into disjoint usage buckets. Malformed detail must not create a
// negative ordinary-input count or an unsupported cache metric, so it falls
// back to treating the total as ordinary input.
func normalizeOpenAIUsage(inputTokens, outputTokens int, details *openAIInputTokensDetails) Usage {
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}

	usage := Usage{InputTokens: inputTokens, OutputTokens: outputTokens}
	if details == nil || details.CachedTokens < 0 || details.CacheWriteTokens < 0 {
		return usage
	}
	if details.CachedTokens+details.CacheWriteTokens > inputTokens {
		return usage
	}

	usage.InputTokens -= details.CachedTokens + details.CacheWriteTokens
	usage.CacheReadTokens = details.CachedTokens
	usage.CacheWriteTokens = details.CacheWriteTokens
	usage.CacheMeasuredPromptTokens = inputTokens
	usage.CacheMeasuredReadTokens = details.CachedTokens
	return usage
}
