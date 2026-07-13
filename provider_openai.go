package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type openaiProvider struct{}

func (openaiProvider) definition() providerDefinition {
	return providerDefinition{UsageURL: "https://chatgpt.com/backend-api/wham/usage"}
}

func (p openaiProvider) fetch(ctx context.Context, client *http.Client, plan planConfig, auth authFile) (usageResponse, error) {
	endpoint := plan.UsageURL
	if endpoint == "" {
		endpoint = p.definition().UsageURL
	}
	body, err := providerGET(ctx, client, endpoint, "Bearer "+auth.AccessToken, map[string]string{"ChatGPT-Account-Id": auth.AccountID})
	if err != nil {
		return usageResponse{}, err
	}

	var upstream upstreamUsage
	if err := json.Unmarshal(body, &upstream); err != nil {
		return usageResponse{}, fmt.Errorf("decode OpenAI usage response: %w", err)
	}
	usage := normalizeUsage(upstream, time.Now().UTC())
	usage.Provider = planProvider(plan)
	usage.UsagePlanName = plan.Plan
	return usage, nil
}
