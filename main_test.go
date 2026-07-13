package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsagePlansAndModelRouting(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		planType := "openai-plan"
		if r.URL.Path == "/anthropic" {
			planType = "anthropic-plan"
		}
		_, _ = w.Write([]byte(`{"plan_type":"` + planType + `","rate_limit":{"allowed":true,"limit_reached":false}}`))
	}))
	defer upstream.Close()

	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"token","account_id":"account"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	service := newUsageServiceWithPlans([]planConfig{
		{Provider: "openai", Plan: "openai_plan_01", Models: []modelMapping{{LiteLLMName: "openai/gpt-5-codex"}}, UsageURL: upstream.URL + "/openai", AuthFile: authPath},
		{Provider: "openai", Plan: "openai_plan_02", Models: []modelMapping{{LiteLLMName: "openai/team"}}, UsageURL: upstream.URL + "/team", AuthFile: authPath},
	}, time.Minute, upstream.Client())
	handler := newHandler("secret", service)

	plansRequest := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	plansRequest.Header.Set("X-Internal-API-Key", "secret")
	plansResult := httptest.NewRecorder()
	handler.ServeHTTP(plansResult, plansRequest)
	var plans usagePlansResponse
	if err := json.Unmarshal(plansResult.Body.Bytes(), &plans); err != nil {
		t.Fatal(err)
	}
	if plansResult.Code != http.StatusOK || len(plans) != 2 || plans[0].Provider != "openai" || plans[0].Usage.PlanType != "openai-plan" || plans[1].Models[0].LiteLLMName != "openai/team" || plans[1].Usage.Provider != "openai" {
		t.Fatalf("unexpected plans response: status=%d body=%s", plansResult.Code, plansResult.Body.String())
	}

	modelRequest := httptest.NewRequest(http.MethodGet, "/v1/usage/openai/team", nil)
	modelRequest.Header.Set("X-Internal-API-Key", "secret")
	modelResult := httptest.NewRecorder()
	handler.ServeHTTP(modelResult, modelRequest)
	var usage usageResponse
	if err := json.Unmarshal(modelResult.Body.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if modelResult.Code != http.StatusOK || usage.ModelID != "openai/team" || usage.Provider != "openai" {
		t.Fatalf("unexpected model response: status=%d body=%s", modelResult.Code, modelResult.Body.String())
	}

	unknownRequest := httptest.NewRequest(http.MethodGet, "/v1/usage/unknown/model", nil)
	unknownRequest.Header.Set("X-Internal-API-Key", "secret")
	unknownResult := httptest.NewRecorder()
	handler.ServeHTTP(unknownResult, unknownRequest)
	if unknownResult.Code != http.StatusNotFound {
		t.Fatalf("unknown model status=%d body=%s", unknownResult.Code, unknownResult.Body.String())
	}
}

func TestValidateProvidersConfigRejectsUnknownProvider(t *testing.T) {
	t.Parallel()

	err := validateProvidersConfig(providersConfig{Plans: []planConfig{{
		Provider: "custom_provider", Plan: "standard", Models: []modelMapping{{LiteLLMName: "custom/model"}}, AuthFile: "/tokens/custom.json",
	}}})
	if err == nil || !strings.Contains(err.Error(), "no built-in definition") {
		t.Fatalf("expected unknown provider error, got %v", err)
	}
}

func TestLoadProviderConfigAndMultiplePlans(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "providers.json")
	configJSON := `{"plans":[{"provider":"openai","plan":"openai_plan_01","models":[{"litellm_name":"openai/free"}],"auth_file":"/tokens/free.json"},{"provider":"openai","plan":"openai_plan_02","models":[{"litellm_name":"openai/team"}],"auth_file":"/tokens/team.json"}]}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := loadProviderConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	plans, err := loadPlans(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].Provider != "openai" || plans[1].Plan != "openai_plan_02" || plans[1].Models[0].LiteLLMName != "openai/team" || plans[1].AuthFile != "/tokens/team.json" {
		t.Fatalf("unexpected plans: %+v", plans)
	}
}

func TestMCPInitializeAndToolCall(t *testing.T) {
	t.Parallel()

	service := &usageService{cache: &cacheEntry{value: usageResponse{PlanType: "pro", RetrievedAt: "2026-07-13T00:00:00Z"}, expires: time.Now().Add(time.Minute)}}
	handler := newHandler("secret", service)

	initializeBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	initialize := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initializeBody))
	initialize.Header.Set("X-Internal-API-Key", "secret")
	initialize.Header.Set("Content-Type", "application/json")
	initializeResult := httptest.NewRecorder()
	handler.ServeHTTP(initializeResult, initialize)
	if initializeResult.Code != http.StatusOK {
		t.Fatalf("initialize status=%d body=%s", initializeResult.Code, initializeResult.Body.String())
	}

	callBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_codex_usage","arguments":{}}}`
	call := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(callBody))
	call.Header.Set("X-Internal-API-Key", "secret")
	call.Header.Set("Content-Type", "application/json")
	callResult := httptest.NewRecorder()
	handler.ServeHTTP(callResult, call)

	var response rpcResponse
	if err := json.Unmarshal(callResult.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if callResult.Code != http.StatusOK || response.Error != nil {
		t.Fatalf("call status=%d body=%s", callResult.Code, callResult.Body.String())
	}
}
