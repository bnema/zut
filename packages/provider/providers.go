package provider

// Provider IDs are compile-time constants used by model metadata, routing,
// discovery, and credential selection. Go has no enum type with string
// interoperability, so untyped constants keep the existing public string
// fields and wire formats while giving provider ownership one source of truth.
const (
	ProviderAnthropic            = "anthropic"
	ProviderOpenAI               = "openai"
	ProviderOpenAICodex          = "openai-codex"
	ProviderOpenAIResponses      = "openai-responses"
	ProviderKimi                 = "kimi"
	ProviderDeepSeek             = "deepseek"
	ProviderGoogle               = "google"
	ProviderOllama               = "ollama"
	ProviderLlamaCPP             = "llama.cpp"
	ProviderMoonshot             = "moonshotai"
	ProviderMoonshotCN           = "moonshotai-cn"
	ProviderCerebras             = "cerebras"
	ProviderGroq                 = "groq"
	ProviderXAI                  = "xai"
	ProviderTogether             = "together"
	ProviderHuggingFace          = "huggingface"
	ProviderOpenRouter           = "openrouter"
	ProviderMistral              = "mistral"
	ProviderZAI                  = "zai"
	ProviderXiaomi               = "xiaomi"
	ProviderXiaomiTokenPlanAMS   = "xiaomi-token-plan-ams"
	ProviderXiaomiTokenPlanCN    = "xiaomi-token-plan-cn"
	ProviderXiaomiTokenPlanSGP   = "xiaomi-token-plan-sgp"
	ProviderMiniMax              = "minimax"
	ProviderMiniMaxCN            = "minimax-cn"
	ProviderFireworks            = "fireworks"
	ProviderVercelAIGateway      = "vercel-ai-gateway"
	ProviderOpenCode             = "opencode"
	ProviderOpenCodeGo           = "opencode-go"
	ProviderAmazonBedrock        = "amazon-bedrock"
	ProviderGoogleVertex         = "google-vertex"
	ProviderAzureOpenAIResponses = "azure-openai-responses"
	ProviderGitHubCopilot        = "github-copilot"
	ProviderCloudflareWorkersAI  = "cloudflare-workers-ai"
	ProviderCloudflareAIGateway  = "cloudflare-ai-gateway"
)

var builtinProviderIDs = []string{
	ProviderAnthropic, ProviderOpenAI, ProviderOpenAICodex, ProviderOpenAIResponses,
	ProviderKimi, ProviderDeepSeek, ProviderGoogle, ProviderOllama, ProviderLlamaCPP,
	ProviderMoonshot, ProviderMoonshotCN, ProviderCerebras, ProviderGroq, ProviderXAI,
	ProviderTogether, ProviderHuggingFace, ProviderOpenRouter, ProviderMistral,
	ProviderZAI, ProviderXiaomi, ProviderXiaomiTokenPlanAMS, ProviderXiaomiTokenPlanCN,
	ProviderXiaomiTokenPlanSGP, ProviderMiniMax, ProviderMiniMaxCN, ProviderFireworks,
	ProviderVercelAIGateway, ProviderOpenCode, ProviderOpenCodeGo, ProviderAmazonBedrock,
	ProviderGoogleVertex, ProviderAzureOpenAIResponses, ProviderGitHubCopilot,
	ProviderCloudflareWorkersAI, ProviderCloudflareAIGateway,
}

// BuiltinProviderIDs returns the canonical built-in provider IDs in fallback
// priority order. The returned slice is independent of package state.
func BuiltinProviderIDs() []string {
	return append([]string(nil), builtinProviderIDs...)
}
