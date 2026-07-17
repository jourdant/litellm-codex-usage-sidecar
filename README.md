# LiteLLM subscription usage sidecar

Small, stateless REST and Streamable HTTP MCP service for provider-specific model usage endpoints. The built-in default uses LiteLLM's `openai` provider and the ChatGPT Codex allowance endpoint.

## Endpoints

- `GET /health`
- `GET /v1/usage`
- `GET /v1/usage?m={model_id},{model_id}`
- `GET /v1/usage/{model_id}`
- `POST /mcp`

`/v1/usage`, `/v1/usage/{model_id}`, and `/mcp` require `X-Internal-API-Key`. The health endpoint intentionally does not.

`GET /v1/usage` returns a standardized envelope with one normalized usage object per configured plan:

```json
{
	"is_success": true,
	"message": "OK",
	"retrieved_at": "2026-07-17T12:34:56Z",
	"data": [
		{
			"provider": "openai",
			"plan_id": "openai_plan_01",
			"is_active": true,
			"is_blocked": false,
			"usage": []
		}
	]
}
```

`GET /v1/usage/{model_id}` resolves the LiteLLM model name to its configured plan, fetches that plan's usage endpoint, and returns the same standardized envelope with a single normalized `data` object. Unknown model names return `404` with the same envelope shape and `is_success: false`.

`GET /v1/usage?m=oai-gpt-5.6-sol,glm-5.2` resolves a comma-delimited list of LiteLLM model names and returns one `data` object per linked provider plan. Each object includes `requested_models`, containing the requested model names linked to that result. Results preserve the order in which each plan first appears, and model names preserve request order within their plan:

```json
{
	"is_success": true,
	"message": "OK",
	"retrieved_at": "2026-07-17T12:34:56Z",
	"data": [
		{
			"provider": "openai",
			"plan_id": "openai_plan_01",
			"requested_models": ["oai-gpt-5.6-sol", "oai-gpt-5.6-luna"],
			"is_active": true,
			"is_blocked": false,
			"usage": []
		},
		{
			"provider": "zai",
			"plan_id": "zai_plan_01",
			"requested_models": ["glm-5.2"],
			"is_active": true,
			"is_blocked": false,
			"usage": []
		}
	]
}
```

The query accepts at most 50 model names. Empty names return `400`, and any unknown model makes the request fail atomically with `404` before provider usage is fetched. Omitting `m` retains the unfiltered one-result-per-configured-plan response.

The service reads each plan's `auth_file` on every uncached retrieval so token refreshes are picked up without restarting. The last successful response is cached in memory for 60 seconds by default per provider plan endpoint. A later request for any other model linked to that same plan reuses the cached upstream result; expired entries trigger a fresh provider request. Set `cache_ttl_seconds` on a subscription usage plan for a per-plan override, or set `CACHE_TTL_SECONDS` for the service-wide fallback.

## Usage response

Example normalized response:

```json
{
	"is_success": true,
	"message": "OK",
	"retrieved_at": "2026-07-11T21:52:54Z"
	"data": {
		"provider": "openai",
		"plan_id": "openai_plan_01",
		"model_id": "oai-gpt-5.5",
		"is_active": true,
		"is_blocked": false,
		"usage": [
			{
				"type": "primary",
				"name": "pro",
				"period": "5-hourly",
				"used_percent": 5,
				"remaining_percent": 95,
				"start_at": "2026-07-11T16:52:54Z",
				"reset_at": "2026-07-11T21:52:54Z"
			}
		]
	}
}
```

The usage window list is standardized across providers. The primary plan uses `type: primary`; any additional usage dimensions (for example a metered feature) use provider-specific identifiers in `type` (for example `codex_bengalfox`). Provider-specific endpoint behavior, expected upstream response shapes, and normalization rules are documented under [Providers](#providers).

## Providers

The sidecar currently implements three providers; their endpoint behavior, expected upstream response shapes, and normalization rules are documented individually:

- [docs/openai.md](docs/openai.md) — ChatGPT Codex WHAM usage endpoint.
- [docs/kimi.md](docs/kimi.md) — Kimi Code membership usage endpoints.
- [docs/zai.md](docs/zai.md) — GLM Coding Plan quota monitor endpoint.

## Provider plans

The read-only LiteLLM YAML is the source of truth for model-to-plan mappings. Define reusable subscription usage objects near the top of `../../data/litellm/config.yaml` and reference them from `model_info.subscription_usage` using YAML anchors. This keeps plan identity and authentication metadata in one place while linking each public LiteLLM `model_name` to its plan:

```yaml
subscription_usage_plans:
  openai: &subscription_usage_openai
    provider: openai
    plan: openai_plan_01
    auth_file: /tokens/auth.json
		cache_ttl_seconds: 90
  zai: &subscription_usage_zai
    provider: zai
    plan: zai_plan_01
    auth_env: Z_AI_API_KEY

model_list:
  - model_name: oai-gpt-5.6-sol
    model_info:
      subscription_usage: *subscription_usage_openai
      mode: responses
    litellm_params:
      model: chatgpt/gpt-5.6-sol

  - model_name: glm-5.2
    model_info:
      subscription_usage: *subscription_usage_zai
    litellm_params:
      model: zai/glm-5.2
```

Models without `model_info.subscription_usage` are ignored. Models that resolve to the same provider and plan are grouped automatically and must resolve to identical subscription metadata. Exactly one authentication source is required. `cache_ttl_seconds` is optional and must be positive when specified; omission uses the service default of 60 seconds. Provider values should be LiteLLM provider IDs. The sidecar currently implements `openai`, `kimi`, and `zai`; provider endpoint behavior remains built into the sidecar.

`plan_details_path` is returned as provider metadata for consumers that need to build a provider-specific plan-details request. It is controlled by the built-in provider adapter rather than the editable plan file. The sidecar currently fetches usage only; it does not fetch or proxy plan details separately.

## Layout

```
main.go                                         # HTTP server, MCP handler, usage service, config loading, normalization
providers/provider.go                           # provider facade, shared types, authentication, and HTTP handling
providers/provider_openai.go                    # OpenAI (ChatGPT Codex WHAM) adapter
providers/provider_kimi.go                      # Kimi (Kimi Code usage) adapter
providers/provider_zai.go                       # z.ai (GLM Coding Plan quota) adapter
../../data/litellm/config.yaml                  # mounted LiteLLM model and subscription metadata
docs/                                           # per-provider documentation (endpoints, response shapes, normalization)
```

Tests mirror this layout: `main_test.go` covers routing, config, and the MCP handler; `usage_service_test.go` covers application-level caching and defaults; provider adapter tests live beside their implementations under `providers/`.

## LiteLLM rollout

The sidecar can run and be tested independently. Adding the REST pass-through and MCP registration to `data/litellm/config.yaml` requires a LiteLLM restart, so those changes are intentionally deferred while the shared proxy is in use.

When a restart window is approved, add `SUBSCRIPTION_USAGE_INTERNAL_KEY=${LITELLM_SALT_KEY}` to the `litellm` service environment and add this configuration:

```yaml
general_settings:
	pass_through_endpoints:
		- path: /subscription-usage
			target: http://litellm-subscription-usage-sidecar:8080
			include_subpath: true
			methods: [GET]
			timeout: 15
			headers:
				X-Internal-API-Key: os.environ/SUBSCRIPTION_USAGE_INTERNAL_KEY

litellm_settings:
	mcp_aliases:
		subscription_usage: subscription_usage_mcp

mcp_servers:
	subscription_usage_mcp:
		url: http://litellm-subscription-usage-sidecar:8080/mcp
		transport: http
		allow_all_keys: true
		allowed_tools: [get_model_usage]
		static_headers:
			X-Internal-API-Key: os.environ/SUBSCRIPTION_USAGE_INTERNAL_KEY
		description: Provider account subscription usage and allowance by LiteLLM model
```

Then add a `litellm-subscription-usage-sidecar` healthy dependency to `litellm`, recreate only the LiteLLM container, and verify `/subscription-usage/v1/usage/{model_id}` plus the `subscription_usage_mcp` tool through LiteLLM authentication.

## Environment

- `INTERNAL_API_KEY` is required for authenticated endpoints.
- `LITELLM_CONFIG_FILE` defaults to `/data/litellm-config.yaml`.
- `CACHE_TTL_SECONDS` defaults to `60` seconds and controls the in-memory provider response cache.
- `UPSTREAM_TIMEOUT_SECONDS` defaults to `10`.
