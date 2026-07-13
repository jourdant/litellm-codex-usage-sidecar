package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type zaiProvider struct{}

type zaiQuotaResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Limits []zaiLimit `json:"limits"`
	} `json:"data"`
	Message string `json:"msg"`
}

type zaiLimit struct {
	Type         string  `json:"type"`
	Unit         int     `json:"unit"`
	Number       int     `json:"number"`
	Percentage   float64 `json:"percentage"`
	CurrentValue float64 `json:"currentValue"`
	Usage        float64 `json:"usage"`
	NextResetAt  int64   `json:"nextResetTime"`
}

func (zaiProvider) definition() providerDefinition {
	return providerDefinition{
		UsageURL:        "https://api.z.ai/api/monitor/usage/quota/limit",
		PlanDetailsPath: "/api/biz/subscription/list",
	}
}

func (p zaiProvider) fetch(ctx context.Context, client *http.Client, plan planConfig, auth authFile) (usageResponse, error) {
	endpoint := plan.UsageURL
	if endpoint == "" {
		endpoint = p.definition().UsageURL
	}
	body, err := providerGET(ctx, client, endpoint, auth.AccessToken, nil)
	if err != nil {
		return usageResponse{}, err
	}

	var upstream zaiQuotaResponse
	if err := json.Unmarshal(body, &upstream); err != nil {
		return usageResponse{}, fmt.Errorf("decode z.ai usage response: %w", err)
	}
	if !upstream.Success && upstream.Message != "" {
		return usageResponse{}, fmt.Errorf("z.ai usage unavailable: %s", upstream.Message)
	}

	usage := usageResponse{
		Provider:      planProvider(plan),
		UsagePlanName: plan.Plan,
		PlanType:      plan.Plan,
		Allowed:       true,
		RetrievedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	for _, limit := range upstream.Data.Limits {
		if limit.Type != "TOKENS_LIMIT" {
			continue
		}
		window := &usageWindow{
			UsedPercent:      clampPercent(limit.Percentage),
			RemainingPercent: 100 - clampPercent(limit.Percentage),
			ResetsAt:         formatEpochMilliseconds(limit.NextResetAt),
		}
		if limit.Unit == 3 {
			usage.Primary = window
		} else if limit.Unit == 6 {
			usage.Secondary = window
		}
	}
	if usage.Primary == nil && usage.Secondary == nil {
		return usageResponse{}, fmt.Errorf("z.ai usage response contained no token limits")
	}
	return usage, nil
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func formatEpochMilliseconds(value int64) string {
	if value <= 0 {
		return ""
	}
	return time.UnixMilli(value).UTC().Format(time.RFC3339)
}
