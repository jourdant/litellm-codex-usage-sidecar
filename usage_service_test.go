package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProviderResponseCacheExpiresAtConfiguredTTL(t *testing.T) {
	t.Parallel()

	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"plan_type":"pro-%d","rate_limit":{"allowed":true,"limit_reached":false}}`, calls)
	}))
	defer upstream.Close()

	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"token","account_id":"account"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	service := newUsageServiceWithPlans([]planConfig{{
		Provider: "openai", Plan: "openai_plan_01",
		Models: []modelMapping{{LiteLLMName: "oai-gpt-5.5"}}, UsageURL: upstream.URL, AuthFile: authPath,
	}}, 20*time.Millisecond, upstream.Client())

	first, err := service.retrieve(context.Background(), "oai-gpt-5.5")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.retrieve(context.Background(), "oai-gpt-5.5")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first.PlanType != "pro-1" || second.PlanType != "pro-1" {
		t.Fatalf("expected cached response: calls=%d first=%+v second=%+v", calls, first, second)
	}

	time.Sleep(30 * time.Millisecond)
	third, err := service.retrieve(context.Background(), "oai-gpt-5.5")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || third.PlanType != "pro-2" {
		t.Fatalf("expected expired cache to refetch: calls=%d third=%+v", calls, third)
	}
}

func TestProviderCacheIsSharedAcrossModelsOnSamePlan(t *testing.T) {
	t.Parallel()

	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false}}`))
	}))
	defer upstream.Close()

	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"token","account_id":"account"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	service := newUsageServiceWithPlans([]planConfig{{
		Provider: "openai", Plan: "openai_plan_01",
		Models:   []modelMapping{{LiteLLMName: "model-one"}, {LiteLLMName: "model-two"}},
		UsageURL: upstream.URL, AuthFile: authPath,
	}}, time.Minute, upstream.Client())

	first, err := service.retrieve(context.Background(), "model-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.retrieve(context.Background(), "model-two")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first.ModelID != "model-one" || second.ModelID != "model-two" || first.PlanType != second.PlanType {
		t.Fatalf("expected shared plan cache: calls=%d first=%+v second=%+v", calls, first, second)
	}
}

func TestProviderCacheTTLDefaultsAndAllowsPlanOverride(t *testing.T) {
	t.Parallel()

	service := newUsageServiceWithPlans(nil, 0, http.DefaultClient)
	if service.ttl != 60*time.Second {
		t.Fatalf("expected default TTL of 60s, got %s", service.ttl)
	}
	if ttl := service.cacheTTL(planConfig{}); ttl != 60*time.Second {
		t.Fatalf("expected default plan TTL of 60s, got %s", ttl)
	}
	if ttl := service.cacheTTL(planConfig{CacheTTLSeconds: 15}); ttl != 15*time.Second {
		t.Fatalf("expected plan TTL of 15s, got %s", ttl)
	}
}

func TestDefaultPlanUsesOpenAIProvider(t *testing.T) {
	t.Parallel()

	service := newUsageService("/unused/auth.json", "https://example.com/usage", time.Minute, http.DefaultClient)
	if len(service.plans) != 1 {
		t.Fatalf("expected one default plan, got %d", len(service.plans))
	}
	plan := service.plans[0]
	if plan.Provider != "openai" || plan.Plan != "openai_plan_01" || plan.AuthFile != "/unused/auth.json" || len(plan.Models) != 1 || plan.Models[0].LiteLLMName != "oai-gpt-5.5" {
		t.Fatalf("unexpected default plan: %+v", plan)
	}
}
