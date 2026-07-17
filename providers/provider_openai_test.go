package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenAIProviderNormalizesUsage(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	usage, err := Fetch(context.Background(), upstream.Client(), Plan{Provider: "openai", Plan: "openai_plan_01", UsageURL: upstream.URL, AuthFile: authPath})
	if err != nil {
		t.Fatal(err)
	}
	if usage.Provider != "openai" || usage.UsagePlanName != "openai_plan_01" || usage.Primary == nil || usage.Primary.RemainingPercent != 64 || usage.Secondary == nil || usage.PlanType != "pro" {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}

func TestOpenAIDefinition(t *testing.T) {
	t.Parallel()

	definition, ok := DefinitionFor("openai")
	if !ok || definition.UsageURL != "https://chatgpt.com/backend-api/wham/usage" {
		t.Fatalf("unexpected definition: ok=%v definition=%+v", ok, definition)
	}
}
