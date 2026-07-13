# Codex usage sidecar

Small, stateless REST and Streamable HTTP MCP service for the ChatGPT Codex allowance endpoint.

## Endpoints

- `GET /health`
- `GET /v1/usage`
- `POST /mcp`

`/v1/usage` and `/mcp` require `X-Internal-API-Key`. The health endpoint intentionally does not.

The service reads `/tokens/auth.json` on every uncached retrieval so token refreshes are picked up without restarting. Successful responses are cached for 30 seconds by default, with concurrent cache misses coalesced into one upstream request.

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
