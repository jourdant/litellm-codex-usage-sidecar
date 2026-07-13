# OpenAI provider

The built-in OpenAI adapter (`provider_openai.go`) fetches account-wide ChatGPT Codex allowance from the undocumented WHAM usage endpoint and normalizes it into the sidecar's common usage schema.

- Endpoint: `GET https://chatgpt.com/backend-api/wham/usage`
- Authentication: `Authorization: Bearer <access_token>` plus `ChatGPT-Account-Id: <account_id>` header, both read from the plan's `auth_file`.

## Expected WHAM response

The adapter expects the raw ChatGPT WHAM usage response to have this shape:

```json
{
	"plan_type": "pro",
	"rate_limit": {
		"allowed": true,
		"limit_reached": false,
		"primary_window": {
			"used_percent": 22,
			"reset_at": 1783904400
		},
		"secondary_window": {
			"used_percent": 43,
			"reset_at": 1784450400
		}
	},
	"additional_rate_limits": [
		{
			"limit_name": "GPT-5.3-Codex-Spark",
			"metered_feature": "codex_bengalfox",
			"rate_limit": {
				"allowed": true,
				"limit_reached": false,
				"primary_window": {
					"used_percent": 0,
					"reset_at": 1784479200
				}
			}
		}
	]
}
```

The adapter uses `plan_type`, the top-level `rate_limit`, and each `additional_rate_limits` entry. For every window it clamps `used_percent` to `0`-`100`, derives `remaining_percent` as `100 - used_percent`, and converts the Unix-seconds `reset_at` value to an RFC 3339 `resets_at` string. `primary_window`, `secondary_window`, and `additional_rate_limits` are optional because the upstream response can omit them during policy changes. Unknown upstream fields are ignored.

## Temporary five-hour-window promotion

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
