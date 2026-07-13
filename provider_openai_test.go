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

func TestRetrieveNormalizesAndCachesUsage(t *testing.T) {
	t.Parallel()

	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("ChatGPT-Account-Id") != "account" {
			t.Fatal("upstream credentials missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":36,"reset_at":1783918800},"secondary_window":{"used_percent":14,"reset_at":1784479200}}}`))
	}))
	defer upstream.Close()

	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"token","account_id":"account"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := newUsageService(authPath, upstream.URL, time.Minute, upstream.Client())
	first, err := svc.retrieve(context.Background(), "oai-gpt-5.5")
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.retrieve(context.Background(), "oai-gpt-5.5")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first.Primary == nil || first.Primary.RemainingPercent != 64 || second.PlanType != "pro" {
		t.Fatalf("unexpected result: calls=%d first=%+v second=%+v", calls, first, second)
	}
}

func TestProviderResponseCacheExpiresAtConfiguredTTL(t *testing.T) {
	t.Parallel()

	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
