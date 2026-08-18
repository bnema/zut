# zut providers

zut ships with built-in providers and a model catalog. You can select models
with `/model`, list them with `zut --list-models`, and add private models in
`$ZUT_HOME/models.json`.

## HTTP proxy

Set one global proxy for zut-managed HTTP and HTTPS traffic with the `http_proxy` key in `$ZUT_HOME/config.json`:

```json
{
  "http_proxy": "http://127.0.0.1:7890"
}
```

zut applies this setting at startup. Existing `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy`, and `https_proxy` environment variables take precedence for their corresponding protocol. Standard `NO_PROXY` and `no_proxy` bypass lists remain effective. Restart zut after changing the setting.

`config.json` is not a credential store. If the proxy URL contains a username or password, prefer setting the proxy environment variables in a protected shell or service configuration rather than saving those credentials in `config.json`.

## Login methods

Use `/login` in interactive mode. Type in either provider picker to filter the list by provider ID or display name.

- `api key`: stores an API key in `$ZUT_HOME/auth.json` when the provider uses a normal key.
- `subscription`: stores OAuth credentials for subscription-backed providers.

Use `/logout` to remove stored credentials.

Some providers need more than a single pasted key. For those providers,
`/login` shows setup instructions instead of opening a localhost browser form.
This avoids broken browser flows in SSH, containers, and `kubectl exec`
sessions.

### Command-backed API keys

A provider can obtain its API key from a password manager or another local program. Configure this directly in `$ZUT_HOME/auth.json`:

```json
{
  "openai": {
    "api_key_command": {
      "program": "op",
      "args": ["read", "op://Work/OpenAI/credential"],
      "timeout_ms": 120000
    }
  }
}
```

Custom provider credentials use the same object under `additional_api_key_creds`:

```json
{
  "additional_api_key_creds": {
    "my-company": {
      "api_key_command": {
        "program": "secret-tool",
        "args": ["read", "my-company-api-key"]
      }
    }
  }
}
```

The program is started directly rather than through a shell. The timeout defaults to 120 seconds. It must write one non-empty line to stdout, with no more than 64 KiB of output. Trailing CR/LF characters are removed. Successful results stay only in process memory and are reused until zut exits.

Command-backed credentials count as logged in without being executed. zut materializes one only when selecting that provider; background model discovery skips it. `/login` with a normal key replaces the command, while `/logout` clears it. Because `auth.json` can cause program execution, keep it user-writable only and do not use files from untrusted sources.

Setup-instruction providers:

- Amazon Bedrock
- Google Vertex AI
- Cloudflare Workers AI
- Cloudflare AI Gateway
- Azure OpenAI Responses

## Subscription providers

These providers support subscription login:

| Provider | Notes |
| --- | --- |
| Anthropic | Claude Pro/Max OAuth credentials. |
| OpenAI Codex | ChatGPT Plus/Pro Codex subscription route. Separate from the OpenAI API-key provider. |
| Kimi | Kimi subscription login. |
| xAI | SuperGrok or X Premium device-code login. The browser URL is prefilled with the device code. |
| GitHub Copilot | GitHub Copilot token flow. |

OAuth tokens are stored in `$ZUT_HOME/auth.json` and refreshed when refresh is
available.

## API-key providers

These providers can use environment variables. Simple API-key providers can
also be configured through `/login`. Providers that require extra cloud setup
show instructions and should be configured with environment variables.

| Provider | Environment variable | Stored key |
| --- | --- | --- |
| Anthropic | `ANTHROPIC_API_KEY` | `anthropic` |
| OpenAI | `OPENAI_API_KEY` | `openai` |
| OpenAI Responses | `OPENAI_API_KEY` | `openai-responses` |
| Kimi | `KIMI_API_KEY` or `MOONSHOT_API_KEY` | `kimi` |
| Google Gemini | `GEMINI_API_KEY` or `GOOGLE_API_KEY` | `google` |
| DeepSeek | `DEEPSEEK_API_KEY` | `deepseek` |
| Moonshot AI | `MOONSHOT_API_KEY` | `moonshotai` |
| Moonshot AI China | `MOONSHOT_API_KEY` | `moonshotai-cn` |
| Groq | `GROQ_API_KEY` | `groq` |
| xAI | `XAI_API_KEY` | `xai` |
| Cerebras | `CEREBRAS_API_KEY` | `cerebras` |
| Together AI | `TOGETHER_API_KEY` | `together` |
| Hugging Face | `HF_TOKEN` | `huggingface` |
| OpenRouter | `OPENROUTER_API_KEY` | `openrouter` |
| Mistral | `MISTRAL_API_KEY` | `mistral` |
| ZAI | `ZAI_API_KEY` | `zai` |
| Xiaomi MiMo | `XIAOMI_API_KEY` | `xiaomi` |
| Xiaomi Token Plan Amsterdam | `XIAOMI_TOKEN_PLAN_AMS_API_KEY` | `xiaomi-token-plan-ams` |
| Xiaomi Token Plan China | `XIAOMI_TOKEN_PLAN_CN_API_KEY` | `xiaomi-token-plan-cn` |
| Xiaomi Token Plan Singapore | `XIAOMI_TOKEN_PLAN_SGP_API_KEY` | `xiaomi-token-plan-sgp` |
| MiniMax | `MINIMAX_API_KEY` | `minimax` |
| MiniMax China | `MINIMAX_CN_API_KEY` or `MINIMAX_API_KEY` | `minimax-cn` |
| Fireworks | `FIREWORKS_API_KEY` | `fireworks` |
| Vercel AI Gateway | `AI_GATEWAY_API_KEY` | `vercel-ai-gateway` |
| OpenCode Zen | `OPENCODE_API_KEY` | `opencode` |
| OpenCode Go | `OPENCODE_API_KEY` | `opencode-go` |
| GitHub Copilot token | `COPILOT_GITHUB_TOKEN` or `GITHUB_COPILOT_TOKEN` | `github-copilot` |
| Cloudflare Workers AI | `CLOUDFLARE_API_KEY` | `cloudflare-workers-ai` |
| Cloudflare AI Gateway | `CLOUDFLARE_API_KEY` | `cloudflare-ai-gateway` |
| Azure OpenAI Responses | `AZURE_OPENAI_API_KEY` | `azure-openai-responses` |

Example:

```bash
export OPENROUTER_API_KEY=...
zut --provider openrouter
```

## Fast mode

Use `/fast` or `/settings` to enable **fast mode** for subsequent model calls.
The setting is off by default and is stored as `fast_mode` in
`$ZUT_HOME/config.json`.

Fast mode currently requests OpenAI's Fast service tier for OpenAI Chat,
OpenAI Responses, and OpenAI Codex requests. Enabling it while using another
provider currently returns an error instead of silently changing that provider's
request. Subagent children inherit the setting from their parent unless their
profile sets `fastMode` explicitly.

## Session identity and caching

zut supplies every provider request with a root cache identity, a
conversation-thread identity, and a per-prompt turn ID. They are opaque local
correlations; adapters translate them only when their exact provider, model,
and endpoint declaration supports it. Resident children share the root cache
identity but retain distinct thread identities. The OpenAI/Codex Responses
route uses the root identity as `prompt_cache_key`; declared Codex routes also
receive their documented session and thread routing headers. A generic
OpenAI-compatible endpoint never receives these extensions merely because it
accepts an OpenAI-shaped request. JSON event mode also reports sanitized cache
diagnostics (`eligible`, `mode`, `transport`, and `continuation`). These
records never include prompts, durable IDs, request bodies, or credentials.

When a provider reports cache detail, zut displays cache-read hit rate as
`cache reads / cache-reporting prompt input`. A shown `0%` is a confirmed
cache miss; no percentage means the provider or historical session did not
report cache detail. Cache writes remain separate from that hit rate. Provider
retention and eviction, request routing, and OpenAI's 1,024-token eligibility
minimum can still produce a miss even when the stable prefix is unchanged.

### OpenAI Responses WebSocket mode

For the exact public endpoint `https://api.openai.com/v1/responses`, zut uses
the Responses API WebSocket mode for the OpenAI Responses route. It keeps one
connection per conversation thread, sends one `response.create` at a time,
and continues the most recent completed response with `previous_response_id`
only when the complete response-context configuration and prior input prefix
match exactly, including the completed assistant output that precedes a new
suffix. Compatible continuations send only new tool or user input.
The request uses `store:false`; its only provider-specific handshake header is
`Authorization: Bearer ...`, never the
ChatGPT/Codex account, routing, or session headers.

The capability is deliberately not inherited by `--base-url`, compatible
providers, Azure, or the ChatGPT Codex subscription endpoint. Those clients
continue to use their declared HTTP/SSE contracts. A request without a logical
session ID, a failed WebSocket setup,
an injected nonstandard RoundTripper, or a configured proxy falls back to
HTTP/SSE with the same logical session/cache identity. Cancellation closes
only the active session socket; a normally completed turn keeps it warm for
the next continuation.


## Local llama.cpp router

The `llama.cpp` provider connects to a multi-model router and is separate from the `ollama` provider. Ollama normally uses `http://localhost:11434`; entering that URL for llama.cpp management produces a 404 because Ollama does not implement the router endpoints.

Install a current llama.cpp build. On macOS:

```bash
brew install llama.cpp
# use `brew upgrade llama.cpp` for an existing installation
```

Start `llama-server` without `--model`, `-m`, or `-hf`. Those flags select one model and disable the router behavior zut expects.

```bash
mkdir -p ~/llama-models

llama-server \
  --models-dir ~/llama-models \
  --no-models-autoload \
  --jinja \
  --host 127.0.0.1 \
  --port 8080 \
  -ngl 999 \
  -c 32768
```

This configuration discovers GGUF files below `~/llama-models`, leaves model loading under explicit user control, enables chat templates, requests GPU offload, and sets a 32K context. Tune the GPU layers and context size for the available memory.

Check the management API before opening zut:

```bash
curl http://127.0.0.1:8080/health
curl http://127.0.0.1:8080/models
```

`/models` must return JSON with a `data` array. A 404 indicates the wrong URL, an older server build, or single-model mode.

To save the connection, run `/login`, choose `api key`, and select `llama.cpp`. Enter `http://127.0.0.1:8080`, without `/v1`, followed by an optional bearer key. zut validates the URL and stores both values in `$ZUT_HOME/auth.json`. Environment variables override the saved connection:

```bash
export LLAMA_BASE_URL=http://127.0.0.1:8080
export LLAMA_API_KEY=optional-secret
```

If a key is configured, start the router with the same `--api-key` value. A server bound only to `127.0.0.1` normally does not need authentication.

After login, `/llama` becomes visible. It shows model state, searches Hugging Face for GGUF repositories, offers available quantizations, reports download and load progress, and explicitly loads or unloads models. Press `d` on a router-downloaded cache model to ask the router to remove it after confirmation. Models discovered through `--models-dir` or a preset are not removable through the router API and must be deleted from their configured source. Some llama.cpp versions retain shared Hugging Face repository artifacts such as `mmproj` files after removing the selected GGUF; remove that repository's cache directory manually if those artifacts are no longer needed. `HF_TOKEN` raises Hugging Face API limits and may be required for repository metadata. For gated files, approve access first and provide the authorized token to the `llama-server` process because the router performs the download.

Opening `/model` refreshes the router and displays its loaded models under provider `llama.cpp`. Unloaded models cannot handle inference and therefore remain exclusive to `/llama` until loaded. Inference is sent to the router's derived `/v1` endpoint.

Ollama downloads are stored in Ollama's internal layout and do not automatically become llama.cpp models. Obtain a GGUF copy through `/llama` or place GGUF files under `~/llama-models`, then restart the router after adding files manually.

## Cloud providers

### Amazon Bedrock

Bedrock is configured with AWS credentials, not a generic zut API-key entry.
Use one of these credential sources:

```bash
# AWS profile
export AWS_PROFILE=your-profile

# IAM access keys
export AWS_ACCESS_KEY_ID=AKIA...
export AWS_SECRET_ACCESS_KEY=...
export AWS_SESSION_TOKEN=... # only for temporary credentials

# Bedrock API key bearer token
export AWS_BEARER_TOKEN_BEDROCK=bedrock-api-key-...

# Region
export AWS_REGION=us-east-1
```

ECS task roles, IRSA, and other AWS SDK credential-chain sources are also
supported.

Example:

```bash
AWS_BEARER_TOKEN_BEDROCK=bedrock-api-key-... AWS_REGION=us-east-1 \
  zut --provider amazon-bedrock --model anthropic.claude-sonnet-4-5-20250929-v1:0
```

Some Bedrock models require regional inference-profile IDs for on-demand
throughput, such as `us.` or `eu.` prefixed model IDs. zut rewrites known
families automatically where possible. Claude Opus 5 uses
`global.anthropic.claude-opus-5`; zut maps the bare foundation-model ID to that
global profile and supports its adaptive reasoning levels. Explicit profile IDs
and ARNs are left unchanged.

### Google Vertex AI

Vertex can use a Google API key when available:

```bash
export GOOGLE_CLOUD_API_KEY=...
zut --provider google-vertex
```

For service-account or application-default credentials, set the standard
Google environment variables used by your deployment.

### Cloudflare AI Gateway

Cloudflare AI Gateway needs a Cloudflare token plus account and gateway IDs:

```bash
export CLOUDFLARE_API_KEY=...
export CLOUDFLARE_ACCOUNT_ID=...
export CLOUDFLARE_GATEWAY_ID=...
zut --provider cloudflare-ai-gateway
```

### Cloudflare Workers AI

Workers AI needs a Cloudflare token and account ID:

```bash
export CLOUDFLARE_API_KEY=...
export CLOUDFLARE_ACCOUNT_ID=...
zut --provider cloudflare-workers-ai
```

### Azure OpenAI Responses

```bash
export AZURE_OPENAI_API_KEY=...
export AZURE_OPENAI_BASE_URL=https://your-resource.openai.azure.com
export AZURE_OPENAI_API_VERSION=v1 # optional, v1 is the default
zut --provider azure-openai-responses
```

The provider uses Azure's Responses API. If deployment names differ from zut
model IDs, map them without changing the catalog:

```bash
export AZURE_OPENAI_DEPLOYMENT_NAME_MAP='gpt-5.6-luna=luna-preview,gpt-5.6-sol=sol-preview'
```

## Auth file

Credentials are stored in `$ZUT_HOME/auth.json` with user-only permissions
when zut creates the file.

Example:

```json
{
  "anthropic": { "api_key": "sk-ant-..." },
  "openai": { "api_key": "sk-..." },
  "google": { "api_key": "..." },
  "additional_api_key_creds": {
    "openrouter": { "api_key": "..." },
    "mistral": { "api_key": "..." }
  }
}
```

The top-level keys are used for providers with dedicated credential fields.
Other API-key providers are stored under `additional_api_key_creds`. Prefer
`/login` so zut writes the correct schema.

## Custom providers and models

Use `$ZUT_HOME/models.json` for private models, deployment aliases, local
servers, or OpenAI-compatible gateways that are not in the built-in catalog.
User entries override built-in entries with the same provider and model ID, and
adding a `models.json` no longer hides the built-in catalog: your entries are
merged on top of the baked-in and live-discovered models.

A top-level provider key that is not a built-in id defines a custom provider.
Give it a provider-level `baseUrl` and an `api` wire format (`openai` for
OpenAI-compatible Chat Completions, the default, `openai-responses` for the
OpenAI Responses API, or `anthropic` for the Anthropic Messages API). A
model-level `baseUrl` overrides the provider-level one for that model, and an
unknown `api` value falls back to `openai` with a warning.

```json
{
  "providers": {
    "my-company": {
      "baseUrl": "https://llm.mycompany.com/v1",
      "api": "openai",
      "models": [
        { "id": "company-llm-v2", "name": "Company LLM v2" }
      ]
    }
  }
}
```

Custom providers are first-class: they appear in `--list-models`, `/model`, and
`/login`. `models.json` never stores secrets. Supply the key through `/login`,
`--api-key`, or a derived environment variable in upper snake case, so
`my-company` reads `MY_COMPANY_API_KEY`. Because many self-hosted gateways do
not expose a model-list endpoint, custom provider keys are accepted and stored
without a verification probe; an invalid key surfaces on the first model call.

To retrieve this custom provider's key from a password manager, add a matching
entry to `$ZUT_HOME/auth.json`:

```json
{
  "additional_api_key_creds": {
    "my-company": {
      "api_key_command": {
        "program": "op",
        "args": ["read", "op://Work/OpenAI/credential"],
        "timeout_ms": 120000
      }
    }
  }
}
```

The provider IDs in `models.json` and `auth.json` must match. Then select the
custom model directly:

```sh
zut --provider my-company --model company-llm-v2
```

## Credential resolution

For each request, zut checks credentials in this order:

1. Explicit CLI key, such as `--api-key`.
2. Provider-specific environment variables (including derived custom-provider
   variables such as `MY_COMPANY_API_KEY`).
3. `$ZUT_HOME/auth.json`, including custom provider keys saved by `/login`.

`models.json` itself never stores credentials; it only describes models and
their endpoints.

Bedrock then uses the AWS SDK credential chain for the actual request.
