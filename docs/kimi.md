# Kimi provider

The built-in Kimi adapter (provider_kimi.go) fetches usage for Kimi Code membership keys.

- Primary usage endpoint: GET https://api.kimi.com/coding/v1/usages
- Optional subscription metadata: GET https://api.kimi.com/api/biz/subscription/list
- Fallback endpoints:
  - GET https://api.kimi.com/coding/v1/dashboard/billing/usage
  - GET https://api.kimi.com/coding/v1/usage
- Authentication: Authorization: Bearer <key>. The adapter also sends x-api-key: <key> for compatibility with Kimi Coding Plan endpoints.

## Supported response shapes

The adapter supports three upstream response families:

0. Kimi Coding usages shape:

```json
{
	"usage": {
		"limit": "100",
		"used": "36",
		"remaining": "64",
		"resetTime": "2026-07-23T22:55:10.633434Z"
	},
	"limits": [
		{
			"window": {
				"duration": 300,
				"timeUnit": "TIME_UNIT_MINUTE"
			},
			"detail": {
				"limit": "100",
				"used": "89",
				"remaining": "11",
				"resetTime": "2026-07-17T13:55:10.633434Z"
			}
		}
	]
}
```

1. Monitor quota shape (same normalized mapping used for z.ai):

```json
{
	"success": true,
	"data": {
		"limits": [
			{
				"type": "TOKENS_LIMIT",
				"unit": 3,
				"percentage": 15,
				"nextResetTime": 1770648402389
			},
			{
				"type": "TOKENS_LIMIT",
				"unit": 6,
				"percentage": 40,
				"nextResetTime": 1771300000000
			}
		]
	}
}
```

2. WHAM-like shape:

```json
{
	"plan_type": "kimi-pro",
	"rate_limit": {
		"allowed": true,
		"limit_reached": false,
		"primary_window": {
			"used_percent": 20,
			"reset_at": 1783918800
		}
	}
}
```

For Kimi usages responses, `usage` is mapped as the weekly window and any `limits[]` entry with `{duration: 300, timeUnit: TIME_UNIT_MINUTE}` is mapped as the five-hour window.

For quota responses, TOKENS_LIMIT windows map to primary (unit 3) and secondary (unit 6), with percentage values clamped to 0-100 and nextResetTime normalized from epoch milliseconds to RFC 3339.

For WHAM-like responses, the adapter uses the same normalization rules as the OpenAI adapter.

Because Kimi usage endpoints are not publicly documented as a stable contract, this adapter intentionally tries multiple endpoint and auth combinations and accepts either supported response shape.
