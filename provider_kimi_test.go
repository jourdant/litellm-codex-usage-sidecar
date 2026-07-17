package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestKimiProviderMapsQuotaWindows(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "kimi-token" {
			t.Fatalf("unexpected kimi authorization: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":12,"nextResetTime":1770648402389},{"type":"TOKENS_LIMIT","unit":6,"percentage":45,"nextResetTime":1771300000000}]}}`))
	}))
	defer upstream.Close()

	authPath := filepath.Join(t.TempDir(), "kimi-auth.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"kimi-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	service := newUsageServiceWithPlans([]planConfig{{
		Provider: "kimi", Plan: "kimi_code_plan_01",
		Models:   []modelMapping{{LiteLLMName: "kimi-k2.7-code"}},
		UsageURL: upstream.URL, AuthFile: authPath,
	}}, time.Minute, upstream.Client())
	usage, err := service.retrieve(context.Background(), "kimi-k2.7-code")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Provider != "kimi" || usage.Primary == nil || usage.Primary.UsedPercent != 12 || usage.Secondary == nil || usage.Secondary.UsedPercent != 45 {
		t.Fatalf("unexpected kimi usage: %+v", usage)
	}
}

func TestKimiProviderParsesWHAMStyleUsage(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer kimi-token" {
			t.Fatalf("unexpected kimi authorization: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"kimi-pro","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":20,"reset_at":1783918800}}}`))
	}))
	defer upstream.Close()

	authPath := filepath.Join(t.TempDir(), "kimi-auth-wham.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"kimi-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	service := newUsageServiceWithPlans([]planConfig{{
		Provider: "kimi", Plan: "kimi_code_plan_01",
		Models:   []modelMapping{{LiteLLMName: "kimi-k3"}},
		UsageURL: upstream.URL, AuthFile: authPath,
	}}, time.Minute, upstream.Client())
	usage, err := service.retrieve(context.Background(), "kimi-k3")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Provider != "kimi" || usage.PlanType != "kimi-pro" || usage.Primary == nil || usage.Primary.UsedPercent != 20 {
		t.Fatalf("unexpected kimi WHAM usage: %+v", usage)
	}
}

func TestKimiProviderParsesCodingUsagesEndpoint(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/coding/v1/usages" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer kimi-token" {
			t.Fatalf("unexpected authorization: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("x-api-key") != "kimi-token" {
			t.Fatalf("missing x-api-key header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"limit":"100","used":"36","remaining":"64","resetTime":"2026-07-23T22:55:10.633434Z"},"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":"100","used":"89","remaining":"11","resetTime":"2026-07-17T13:55:10.633434Z"}}]}`))
	}))
	defer upstream.Close()

	authPath := filepath.Join(t.TempDir(), "kimi-auth-usages.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"kimi-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	service := newUsageServiceWithPlans([]planConfig{{
		Provider: "kimi", Plan: "kimi_code_plan_01",
		Models:   []modelMapping{{LiteLLMName: "kimi-3"}},
		UsageURL: upstream.URL + "/coding/v1/usages", AuthFile: authPath,
	}}, time.Minute, upstream.Client())
	usage, err := service.retrieve(context.Background(), "kimi-3")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Provider != "kimi" || usage.Primary == nil || usage.Secondary == nil {
		t.Fatalf("unexpected usage windows: %+v", usage)
	}
	if usage.Primary.UsedPercent != 89 || usage.Secondary.UsedPercent != 36 {
		t.Fatalf("unexpected usage percentages: %+v", usage)
	}
}
