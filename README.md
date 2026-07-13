# LiteLLM usage sidecar

Small, stateless REST and Streamable HTTP MCP service for provider-specific model usage endpoints. The built-in default uses LiteLLM's `openai` provider and the ChatGPT Codex allowance endpoint.

## Endpoints

- `GET /health`
- `GET /v1/usage`
- `GET /v1/usage/{model_id}`
- `POST /mcp`

`/v1/usage`, `/v1/usage/{model_id}`, and `/mcp` require `X-Internal-API-Key`. The health endpoint intentionally does not.

`GET /v1/usage` returns an array of configured provider plans. Each entry includes the explicit LiteLLM-to-provider model mappings and the current usage data for that plan:

```json
[
	{
		"provider": "openai",
		"plan": "openai_plan_01",
		"models": [
			{"litellm_name": "oai-gpt-5.5"},
			{"litellm_name": "oai-gpt-5.6-luna"}
		],
		"usage_path": "/v1/usage/{model_id}",
		"usage": {
			"provider": "openai",
			"usage_plan_name": "openai_plan_01",
			"plan_type": "pro",
			"allowed": true,
			"limit_reached": false
		}
	}
]
```

`GET /v1/usage/{model_id}` resolves the LiteLLM model name to its configured plan, fetches that plan's usage endpoint, and returns the focused normalized usage response with `model_id`, `provider`, and `usage_plan_name` metadata. Unknown model names return `404`.

The service reads each plan's `auth_file` on every uncached retrieval so token refreshes are picked up without restarting. The last successful response is cached in memory for 60 seconds by default per provider plan endpoint. Requests for multiple LiteLLM model names that resolve to the same plan reuse that response; expired entries trigger a fresh provider request. Set `CACHE_TTL_SECONDS` to change the cache duration.

## Usage response

Example normalized response:

```json
{
	"plan_type": "pro",
	"allowed": true,
	"limit_reached": false,
	"primary": {
		"used_percent": 5,
		"remaining_percent": 95,
		"resets_at": "2026-07-11T21:52:54Z"
	},
	"additional_limits": [
		{
			"name": "GPT-5.3-Codex-Spark",
			"metered_feature": "codex_bengalfox",
			"allowed": true,
			"limit_reached": false,
			"primary": {
				"used_percent": 0,
				"remaining_percent": 100,
				"resets_at": "2026-07-20T11:23:22Z"
			}
		}
	],
	"retrieved_at": "2026-07-11T21:52:54Z"
}
```

The meaning of `primary` is determined by the upstream window duration, not by its name in this normalized response. Provider-specific endpoint behavior, expected upstream response shapes, and normalization rules are documented under [Providers](#providers).

## Providers

The sidecar currently implements two providers; their endpoint behavior, expected upstream response shapes, and normalization rules are documented individually:

- [docs/openai.md](docs/openai.md) — ChatGPT Codex WHAM usage endpoint.
- [docs/zai.md](docs/zai.md) — GLM Coding Plan quota monitor endpoint.

## Provider plans

Edit `config/providers.json` to configure providers and multiple plans. A provider can have multiple plans, and each plan owns its own identity (`auth_file`) and model entries. Each model entry is intentionally an object containing LiteLLM's public `model_name` as `litellm_name`, leaving room for future model-specific metadata. No provider model ID is required because the WHAM response reports account-wide usage rather than per-model limits, and wildcard entries are not supported. There is no separate plan ID: the provider and plan name identify a configured usage source, and each LiteLLM model name must appear in at most one plan. Provider endpoint behavior is built into the sidecar, so changing provider URLs is not required for the supported provider implementation:

```json
{
	"plans": [
		{
			"provider": "openai",
			"plan": "openai_plan_01",
			"models": [
				{"litellm_name": "oai-gpt-5.5"},
				{"litellm_name": "oai-gpt-5.6-luna"}
			],
			"auth_file": "/tokens/openai-default.json"
		},
		{
			"provider": "openai",
			"plan": "openai_plan_02",
			"models": [
				{"litellm_name": "oai-gpt-5.6-sol"}
			],
			"auth_file": "/tokens/openai-team.json"
		}
	]
}
```

Provider values should be LiteLLM provider IDs. The sidecar currently implements `openai` and `zai`; provider-specific endpoint behavior is built into the sidecar, while plan identities and LiteLLM model mappings are user configuration. The OpenAI adapter is in `provider_openai.go` and the z.ai adapter is in `provider_zai.go`.

`plan_details_path` is returned as provider metadata for consumers that need to build a provider-specific plan-details request. It is controlled by the built-in provider adapter rather than the editable plan file. The sidecar currently fetches usage only; it does not fetch or proxy plan details separately.

## Layout

```
main.go               # HTTP server, MCP handler, usage service, config loading, normalization
provider.go           # provider adapter interface and dispatch
provider_openai.go    # OpenAI (ChatGPT Codex WHAM) adapter
provider_zai.go       # z.ai (GLM Coding Plan quota) adapter
config/providers.json # provider/plan/model configuration
docs/                 # per-provider documentation (endpoints, response shapes, normalization)
```

Tests mirror this layout: `main_test.go` covers routing, config, and the MCP handler; `provider_openai_test.go` and `provider_zai_test.go` cover the adapter fetch paths.

## LiteLLM rollout

The sidecar can run and be tested independently. Adding the REST pass-through and MCP registration to `data/litellm/config.yaml` requires a LiteLLM restart, so those changes are intentionally deferred while the shared proxy is in use.

When a restart window is approved, add `CODEX_USAGE_INTERNAL_KEY=${LITELLM_SALT_KEY}` to the `litellm` service environment and add this configuration:

```yaml
general_settings:
	pass_through_endpoints:
		- path: /codex-usage
			target: http://codex-usage:8080
			include_subpath: true
			methods: [GET]
			timeout: 15
			headers:
				X-Internal-API-Key: os.environ/CODEX_USAGE_INTERNAL_KEY

litellm_settings:
	mcp_aliases:
		codex_usage: codex_usage_mcp

mcp_servers:
	codex_usage_mcp:
		url: http://codex-usage:8080/mcp
		transport: http
		allow_all_keys: true
		allowed_tools: [get_codex_usage]
		static_headers:
			X-Internal-API-Key: os.environ/CODEX_USAGE_INTERNAL_KEY
		description: Codex account usage and allowance
```

Then add a `codex-usage` healthy dependency to `litellm`, recreate only the LiteLLM container, and verify `/codex-usage/v1/usage` plus the `codex_usage_mcp` tool through LiteLLM authentication.

## Environment

- `INTERNAL_API_KEY` is required for authenticated endpoints.
- `PROVIDER_CONFIG_FILE` defaults to `/config/providers.json`.
- `CACHE_TTL_SECONDS` defaults to `60` seconds and controls the in-memory provider response cache.
- `UPSTREAM_TIMEOUT_SECONDS` defaults to `10`.
