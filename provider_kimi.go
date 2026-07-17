package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type kimiProvider struct{}

type kimiQuotaResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Limits []zaiLimit `json:"limits"`
	} `json:"data"`
	Message string `json:"msg"`
}

type kimiCreditGrantsResponse struct {
	Object         string  `json:"object"`
	TotalGranted   float64 `json:"total_granted"`
	TotalUsed      float64 `json:"total_used"`
	TotalAvailable float64 `json:"total_available"`
}

type kimiUsagesResponse struct {
	Usage struct {
		Limit     string `json:"limit"`
		Used      string `json:"used"`
		Remaining string `json:"remaining"`
		ResetTime string `json:"resetTime"`
	} `json:"usage"`
	Limits []struct {
		Window struct {
			Duration int    `json:"duration"`
			TimeUnit string `json:"timeUnit"`
		} `json:"window"`
		Detail struct {
			Limit     string `json:"limit"`
			Used      string `json:"used"`
			Remaining string `json:"remaining"`
			ResetTime string `json:"resetTime"`
		} `json:"detail"`
	} `json:"limits"`
}

func (kimiProvider) definition() providerDefinition {
	return providerDefinition{
		UsageURL:        "https://api.kimi.com/coding/v1/usages",
		PlanDetailsPath: "/api/biz/subscription/list",
	}
}

func (p kimiProvider) fetch(ctx context.Context, client *http.Client, plan planConfig, auth authFile) (usageResponse, error) {
	endpoints := p.candidateEndpoints(plan)
	errs := make([]string, 0, len(endpoints)*2)

	for _, endpoint := range endpoints {
		if usage, ok := p.fromUsagesEndpoint(ctx, client, plan, auth.AccessToken, endpoint); ok {
			return usage, nil
		}

		for _, authorization := range []string{"Bearer " + auth.AccessToken, auth.AccessToken} {
			body, err := providerGET(ctx, client, endpoint, authorization, nil)
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}

			if usage, ok := p.fromQuota(plan, body); ok {
				return usage, nil
			}
			if usage, ok := p.fromWHAM(plan, body); ok {
				return usage, nil
			}
			if usage, ok := p.fromCreditGrants(plan, body); ok {
				return usage, nil
			}

			errs = append(errs, fmt.Sprintf("unrecognized response shape from %s", endpoint))
		}
	}

	if p.keyIsValidForCodingAPI(ctx, client, auth.AccessToken) {
		return usageResponse{
			Provider:      planProvider(plan),
			UsagePlanName: plan.Plan,
			PlanType:      plan.Plan,
			Allowed:       true,
			LimitReached:  false,
			RetrievedAt:   time.Now().UTC().Format(time.RFC3339),
		}, nil
	}

	return usageResponse{}, fmt.Errorf("kimi usage unavailable: %s", strings.Join(errs, "; "))
}

func (p kimiProvider) candidateEndpoints(plan planConfig) []string {
	endpoint := strings.TrimSpace(plan.UsageURL)
	if endpoint == "" {
		endpoint = p.definition().UsageURL
	}
	endpoints := []string{endpoint}
	for _, fallback := range []string{
		"https://api.kimi.com/coding/v1/usages",
		"https://api.kimi.com/coding/v1/dashboard/billing/usage",
		"https://api.kimi.com/coding/v1/usage",
		"https://api.kimi.com/coding/v1/dashboard/billing/credit_grants",
		"https://api.kimi.com/coding/v1/dashboard/billing/subscription",
		"https://api.moonshot.ai/v1/dashboard/billing/credit_grants",
		"https://api.moonshot.ai/v1/dashboard/billing/subscription",
		"https://api.moonshot.ai/v1/dashboard/billing/usage",
	} {
		if fallback != endpoint {
			endpoints = append(endpoints, fallback)
		}
	}
	return endpoints
}

func (kimiProvider) fromQuota(plan planConfig, body []byte) (usageResponse, bool) {
	var upstream kimiQuotaResponse
	if err := json.Unmarshal(body, &upstream); err != nil {
		return usageResponse{}, false
	}
	if !upstream.Success {
		return usageResponse{}, false
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
		return usageResponse{}, false
	}
	return usage, true
}

func (kimiProvider) fromWHAM(plan planConfig, body []byte) (usageResponse, bool) {
	var upstream upstreamUsage
	if err := json.Unmarshal(body, &upstream); err != nil {
		return usageResponse{}, false
	}
	if upstream.PlanType == "" && upstream.RateLimit.PrimaryWindow == nil && upstream.RateLimit.SecondaryWindow == nil {
		return usageResponse{}, false
	}
	usage := normalizeUsage(upstream, time.Now().UTC())
	usage.Provider = planProvider(plan)
	usage.UsagePlanName = plan.Plan
	if usage.PlanType == "" {
		usage.PlanType = plan.Plan
	}
	return usage, true
}

func (kimiProvider) fromCreditGrants(plan planConfig, body []byte) (usageResponse, bool) {
	var grants kimiCreditGrantsResponse
	if err := json.Unmarshal(body, &grants); err != nil {
		return usageResponse{}, false
	}
	if grants.Object == "" && grants.TotalGranted == 0 && grants.TotalUsed == 0 && grants.TotalAvailable == 0 {
		return usageResponse{}, false
	}

	total := grants.TotalGranted
	if total <= 0 {
		total = grants.TotalUsed + grants.TotalAvailable
	}
	if total <= 0 {
		return usageResponse{}, false
	}
	usedPercent := clampPercent((grants.TotalUsed / total) * 100)

	usage := usageResponse{
		Provider:      planProvider(plan),
		UsagePlanName: plan.Plan,
		PlanType:      plan.Plan,
		Allowed:       true,
		LimitReached:  grants.TotalAvailable <= 0,
		Primary: &usageWindow{
			UsedPercent:      usedPercent,
			RemainingPercent: 100 - usedPercent,
		},
		RetrievedAt: time.Now().UTC().Format(time.RFC3339),
	}
	return usage, true
}

func (kimiProvider) keyIsValidForCodingAPI(ctx context.Context, client *http.Client, token string) bool {
	body, err := providerGET(ctx, client, "https://api.kimi.com/coding/v1/models", "Bearer "+token, map[string]string{"x-api-key": token})
	if err != nil {
		return false
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	return len(payload.Data) > 0
}

func (p kimiProvider) fromUsagesEndpoint(ctx context.Context, client *http.Client, plan planConfig, token, endpoint string) (usageResponse, bool) {
	if !strings.HasSuffix(endpoint, "/coding/v1/usages") {
		return usageResponse{}, false
	}
	body, err := providerGET(ctx, client, endpoint, "Bearer "+token, map[string]string{"x-api-key": token})
	if err != nil {
		return usageResponse{}, false
	}

	var upstream kimiUsagesResponse
	if err := json.Unmarshal(body, &upstream); err != nil {
		return usageResponse{}, false
	}
	weeklyUsed, weeklyRemaining, weeklyTotal, ok := parseLimitTuple(upstream.Usage.Used, upstream.Usage.Remaining, upstream.Usage.Limit)
	if !ok || weeklyTotal <= 0 {
		return usageResponse{}, false
	}

	usage := usageResponse{
		Provider:      planProvider(plan),
		UsagePlanName: plan.Plan,
		PlanType:      plan.Plan,
		Allowed:       true,
		LimitReached:  weeklyRemaining <= 0,
		RetrievedAt:   time.Now().UTC().Format(time.RFC3339),
		Secondary: &usageWindow{
			UsedPercent:      clampPercent((weeklyUsed / weeklyTotal) * 100),
			RemainingPercent: clampPercent((weeklyRemaining / weeklyTotal) * 100),
			ResetsAt:         normalizeRFC3339(upstream.Usage.ResetTime),
		},
	}

	for _, limit := range upstream.Limits {
		used, remaining, total, ok := parseLimitTuple(limit.Detail.Used, limit.Detail.Remaining, limit.Detail.Limit)
		if !ok || total <= 0 {
			continue
		}
		if limit.Window.Duration == 300 && strings.EqualFold(limit.Window.TimeUnit, "TIME_UNIT_MINUTE") {
			usage.Primary = &usageWindow{
				UsedPercent:      clampPercent((used / total) * 100),
				RemainingPercent: clampPercent((remaining / total) * 100),
				ResetsAt:         normalizeRFC3339(limit.Detail.ResetTime),
			}
			break
		}
	}

	return usage, true
}

func parseLimitTuple(usedStr, remainingStr, limitStr string) (used, remaining, limit float64, ok bool) {
	used, errUsed := strconv.ParseFloat(strings.TrimSpace(usedStr), 64)
	remaining, errRemaining := strconv.ParseFloat(strings.TrimSpace(remainingStr), 64)
	limit, errLimit := strconv.ParseFloat(strings.TrimSpace(limitStr), 64)
	if errUsed != nil || errRemaining != nil || errLimit != nil {
		return 0, 0, 0, false
	}
	return used, remaining, limit, true
}

func normalizeRFC3339(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, trimmed)
	if err != nil {
		return trimmed
	}
	return parsed.UTC().Format(time.RFC3339)
}
