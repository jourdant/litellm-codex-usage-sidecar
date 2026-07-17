package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type openaiProvider struct{}

func (openaiProvider) definition() Definition {
	return Definition{UsageURL: "https://chatgpt.com/backend-api/wham/usage"}
}

func (p openaiProvider) fetch(ctx context.Context, client *http.Client, plan Plan, auth authFile) (UsageResponse, error) {
	endpoint := plan.UsageURL
	if endpoint == "" {
		endpoint = p.definition().UsageURL
	}
	body, err := providerGET(ctx, client, endpoint, "Bearer "+auth.AccessToken, map[string]string{"ChatGPT-Account-Id": auth.AccountID})
	if err != nil {
		return UsageResponse{}, err
	}
	var upstream upstreamUsage
	if err := json.Unmarshal(body, &upstream); err != nil {
		return UsageResponse{}, fmt.Errorf("decode OpenAI usage response: %w", err)
	}
	usage := normalizeUsage(upstream, time.Now().UTC())
	usage.Provider = normalizeProvider(plan.Provider)
	usage.UsagePlanName = plan.Plan
	return usage, nil
}
