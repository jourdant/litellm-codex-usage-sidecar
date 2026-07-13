# z.ai provider

The built-in z.ai adapter (`provider_zai.go`) fetches GLM Coding Plan quota usage from Z.ai's undocumented monitor endpoints.

- Usage: `GET https://api.z.ai/api/monitor/usage/quota/limit`
- Optional subscription metadata: `GET https://api.z.ai/api/biz/subscription/list`
- Authentication: `Authorization: <z.ai API key>` (the `access_token` field of the plan's `auth_file`).

## Quota response shape

```json
{
	"success": true,
	"data": {
		"limits": [
			{
				"type": "TOKENS_LIMIT",
				"unit": 3,
				"number": 5,
				"percentage": 15,
				"nextResetTime": 1770648402389
			},
			{
				"type": "TOKENS_LIMIT",
				"unit": 6,
				"number": 1,
				"percentage": 40,
				"nextResetTime": 1771300000000
			},
			{
				"type": "TIME_LIMIT",
				"currentValue": 10,
				"usage": 100
			}
		]
	},
	"msg": ""
}
```

The adapter only reads `TOKENS_LIMIT` entries and maps them by window:

- `unit: 3` becomes the primary five-hour window.
- `unit: 6` becomes the secondary weekly window.

Each window's `percentage` becomes `used_percent` (clamped to `0`-`100`), `remaining_percent` is `100 - used_percent`, and `nextResetTime` (epoch milliseconds) is normalized to an RFC 3339 `resets_at` string. `TIME_LIMIT` entries are ignored. If the response contains no `TOKENS_LIMIT` entries the adapter returns an error.

The current adapter uses the quota endpoint and the configured plan name; `PlanDetailsPath` points at the subscription endpoint for future plan-name enrichment, but the sidecar does not fetch plan details separately yet. These are undocumented monitor APIs used by Z.ai's own usage tooling, so their response shape may change.
