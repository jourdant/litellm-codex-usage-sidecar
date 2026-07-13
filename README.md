# Codex usage sidecar

Small, stateless REST and Streamable HTTP MCP service for the ChatGPT Codex allowance endpoint.

## Endpoints

- `GET /health`
- `GET /v1/usage`
- `POST /mcp`

`/v1/usage` and `/mcp` require `X-Internal-API-Key`. The health endpoint intentionally does not.

The service reads `/tokens/auth.json` on every uncached retrieval so token refreshes are picked up without restarting. Successful responses are cached for 30 seconds by default, with concurrent cache misses coalesced into one upstream request.

## Usage response

Example response observed while the account-wide five-hour restriction is temporarily disabled:

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

The meaning of `primary` is determined by the upstream window duration, not by its name in this normalized response. During the temporary promotion, the upstream account response can omit the 18,000-second window and return only the 604,800-second weekly window. In that case the weekly window appears as `primary` and `secondary` is omitted.

Tibo announced that OpenAI was temporarily removing the five-hour usage restriction for Plus, Business, and Pro plans. Search-indexed copies of the announcement also report weekly limits being 50% higher through July 19. Treat this as a temporary server-side policy rather than a stable API contract. See [Tibo's posts and replies](https://x.com/thsottiaux/with_replies) and the [current Codex pricing documentation](https://learn.chatgpt.com/docs/pricing).

When the normal five-hour plus weekly policy returns, the sidecar is expected to emit both windows:

```json
{
	"plan_type": "pro",
	"allowed": true,
	"limit_reached": false,
	"primary": {
		"used_percent": 22,
		"remaining_percent": 78,
		"resets_at": "2026-07-13T05:00:00Z"
	},
	"secondary": {
		"used_percent": 43,
		"remaining_percent": 57,
		"resets_at": "2026-07-19T14:00:00Z"
	},
	"retrieved_at": "2026-07-13T00:00:00Z"
}
```

The corresponding upstream `wham/usage` shape uses window durations to identify each limit:

```json
{
	"plan_type": "pro",
	"rate_limit": {
		"allowed": true,
		"limit_reached": false,
		"primary_window": {
			"used_percent": 22,
			"reset_at": 1783904400,
			"limit_window_seconds": 18000
		},
		"secondary_window": {
			"used_percent": 43,
			"reset_at": 1784450400,
			"limit_window_seconds": 604800
		}
	}
}
```

In the usual upstream schema, `18000` seconds is the five-hour session window and `604800` seconds is the weekly window. This shape is also covered by the open-source [CodexBar Codex OAuth documentation](https://github.com/steipete/CodexBar/blob/main/docs/codex-oauth.md) and its parser tests. The upstream endpoint is undocumented and may change without notice; consumers should tolerate either window being absent.

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
