package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

type Definition struct {
	UsageURL        string
	PlanDetailsPath string
}

type Plan struct {
	Provider string
	Plan     string
	UsageURL string
	AuthFile string
	AuthEnv  string
}

type UsageWindow struct {
	UsedPercent      float64 `json:"used_percent"`
	RemainingPercent float64 `json:"remaining_percent"`
	ResetsAt         string  `json:"resets_at,omitempty"`
}

type UsageLimit struct {
	Name           string       `json:"name"`
	MeteredFeature string       `json:"metered_feature,omitempty"`
	Allowed        bool         `json:"allowed"`
	LimitReached   bool         `json:"limit_reached"`
	Primary        *UsageWindow `json:"primary,omitempty"`
	Secondary      *UsageWindow `json:"secondary,omitempty"`
}

type UsageResponse struct {
	ModelID          string       `json:"model_id,omitempty"`
	Provider         string       `json:"provider,omitempty"`
	UsagePlanName    string       `json:"usage_plan_name,omitempty"`
	PlanType         string       `json:"plan_type"`
	Allowed          bool         `json:"allowed"`
	LimitReached     bool         `json:"limit_reached"`
	Primary          *UsageWindow `json:"primary,omitempty"`
	Secondary        *UsageWindow `json:"secondary,omitempty"`
	AdditionalLimits []UsageLimit `json:"additional_limits,omitempty"`
	RetrievedAt      string       `json:"retrieved_at"`
}

type authFile struct {
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
}

type upstreamWindow struct {
	UsedPercent float64 `json:"used_percent"`
	ResetAt     int64   `json:"reset_at"`
}

type upstreamRateLimit struct {
	Allowed         bool            `json:"allowed"`
	LimitReached    bool            `json:"limit_reached"`
	PrimaryWindow   *upstreamWindow `json:"primary_window"`
	SecondaryWindow *upstreamWindow `json:"secondary_window"`
}

type upstreamAdditionalLimit struct {
	LimitName      string            `json:"limit_name"`
	MeteredFeature string            `json:"metered_feature"`
	RateLimit      upstreamRateLimit `json:"rate_limit"`
}

type upstreamUsage struct {
	PlanType             string                    `json:"plan_type"`
	RateLimit            upstreamRateLimit         `json:"rate_limit"`
	AdditionalRateLimits []upstreamAdditionalLimit `json:"additional_rate_limits"`
}

type adapter interface {
	definition() Definition
	fetch(context.Context, *http.Client, Plan, authFile) (UsageResponse, error)
}

func DefinitionFor(provider string) (Definition, bool) {
	providerAdapter, ok := adapterFor(provider)
	if !ok {
		return Definition{}, false
	}
	return providerAdapter.definition(), true
}

func Fetch(ctx context.Context, client *http.Client, plan Plan) (UsageResponse, error) {
	providerAdapter, ok := adapterFor(plan.Provider)
	if !ok {
		return UsageResponse{}, fmt.Errorf("provider %q has no built-in adapter", plan.Provider)
	}
	auth, err := readPlanAuth(plan)
	if err != nil {
		return UsageResponse{}, err
	}
	return providerAdapter.fetch(ctx, client, plan, auth)
}

func adapterFor(provider string) (adapter, bool) {
	switch normalizeProvider(provider) {
	case "openai":
		return openaiProvider{}, true
	case "kimi":
		return kimiProvider{}, true
	case "zai":
		return zaiProvider{}, true
	default:
		return nil, false
	}
}

func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

func readAuthFile(path string) (authFile, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return authFile{}, fmt.Errorf("read auth file: %w", err)
	}
	var auth authFile
	if err := json.Unmarshal(contents, &auth); err != nil {
		return authFile{}, fmt.Errorf("parse auth file: %w", err)
	}
	if strings.TrimSpace(auth.AccessToken) == "" {
		return authFile{}, fmt.Errorf("auth file is missing access_token")
	}
	return auth, nil
}

func readPlanAuth(plan Plan) (authFile, error) {
	if plan.AuthEnv != "" {
		value := strings.TrimSpace(os.Getenv(plan.AuthEnv))
		if value == "" {
			return authFile{}, fmt.Errorf("environment variable %s is empty", plan.AuthEnv)
		}
		return authFile{AccessToken: value}, nil
	}
	return readAuthFile(plan.AuthFile)
}

func providerGET(ctx context.Context, client *http.Client, endpoint, authorization string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create provider request: %w", err)
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "litellm-subscription-usage-sidecar/1")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request provider usage: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("provider endpoint returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read provider response: %w", err)
	}
	return body, nil
}

func normalizeUsage(upstream upstreamUsage, retrievedAt time.Time) UsageResponse {
	additional := make([]UsageLimit, 0, len(upstream.AdditionalRateLimits))
	for _, limit := range upstream.AdditionalRateLimits {
		additional = append(additional, UsageLimit{
			Name: limit.LimitName, MeteredFeature: limit.MeteredFeature,
			Allowed: limit.RateLimit.Allowed, LimitReached: limit.RateLimit.LimitReached,
			Primary: normalizeWindow(limit.RateLimit.PrimaryWindow), Secondary: normalizeWindow(limit.RateLimit.SecondaryWindow),
		})
	}
	return UsageResponse{
		PlanType: upstream.PlanType, Allowed: upstream.RateLimit.Allowed, LimitReached: upstream.RateLimit.LimitReached,
		Primary: normalizeWindow(upstream.RateLimit.PrimaryWindow), Secondary: normalizeWindow(upstream.RateLimit.SecondaryWindow),
		AdditionalLimits: additional, RetrievedAt: retrievedAt.Format(time.RFC3339),
	}
}

func normalizeWindow(window *upstreamWindow) *UsageWindow {
	if window == nil {
		return nil
	}
	used := math.Max(0, math.Min(100, window.UsedPercent))
	reset := ""
	if window.ResetAt > 0 {
		reset = time.Unix(window.ResetAt, 0).UTC().Format(time.RFC3339)
	}
	return &UsageWindow{UsedPercent: used, RemainingPercent: 100 - used, ResetsAt: reset}
}
