package provider

// openAIInputTokensDetails is shared by the Chat Completions and Responses
// streams. The details object is optional; its presence is the availability
// signal for cache accounting, including a known cache miss.
type openAIInputTokensDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// normalizeDeepSeekUsage converts DeepSeek's explicit hit/miss counts into
// disjoint usage buckets. Cache accounting is accepted only when both fields
// are present and add up to the reported prompt total.
func normalizeDeepSeekUsage(inputTokens, outputTokens int, cacheHits, cacheMisses *int) Usage {
	usage := Usage{InputTokens: inputTokens, OutputTokens: outputTokens}
	if inputTokens < 0 || outputTokens < 0 || cacheHits == nil || cacheMisses == nil || *cacheHits < 0 || *cacheMisses < 0 || *cacheHits+*cacheMisses != inputTokens {
		if usage.InputTokens < 0 {
			usage.InputTokens = 0
		}
		if usage.OutputTokens < 0 {
			usage.OutputTokens = 0
		}
		return usage
	}

	usage.InputTokens = *cacheMisses
	usage.CacheReadTokens = *cacheHits
	usage.CacheMeasuredPromptTokens = inputTokens
	usage.CacheMeasuredReadTokens = *cacheHits
	return usage
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
