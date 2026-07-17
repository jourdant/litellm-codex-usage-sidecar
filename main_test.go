package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
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
	var plans apiUsageListEnvelope
	if err := json.Unmarshal(plansResult.Body.Bytes(), &plans); err != nil {
		t.Fatal(err)
	}
	if plansResult.Code != http.StatusOK || !plans.IsSuccess || len(plans.Data) != 2 || plans.Data[0].Provider != "openai" || plans.Data[0].PlanID != "openai_plan_01" || plans.Data[1].PlanID != "openai_plan_02" {
		t.Fatalf("unexpected plans response: status=%d body=%s", plansResult.Code, plansResult.Body.String())
	}

	modelRequest := httptest.NewRequest(http.MethodGet, "/v1/usage/openai/team", nil)
	modelRequest.Header.Set("X-Internal-API-Key", "secret")
	modelResult := httptest.NewRecorder()
	handler.ServeHTTP(modelResult, modelRequest)
	var usage apiUsageEnvelope
	if err := json.Unmarshal(modelResult.Body.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if modelResult.Code != http.StatusOK || !usage.IsSuccess || usage.Data.ModelID != "openai/team" || usage.Data.Provider != "openai" || usage.Data.PlanID != "openai_plan_02" {
		t.Fatalf("unexpected model response: status=%d body=%s", modelResult.Code, modelResult.Body.String())
	}

	unknownRequest := httptest.NewRequest(http.MethodGet, "/v1/usage/unknown/model", nil)
	unknownRequest.Header.Set("X-Internal-API-Key", "secret")
	unknownResult := httptest.NewRecorder()
	handler.ServeHTTP(unknownResult, unknownRequest)
	var unknown apiErrorEnvelope
	if err := json.Unmarshal(unknownResult.Body.Bytes(), &unknown); err != nil {
		t.Fatal(err)
	}
	if unknownResult.Code != http.StatusNotFound || unknown.IsSuccess || unknown.Message == "" {
		t.Fatalf("unknown model status=%d body=%s", unknownResult.Code, unknownResult.Body.String())
	}
}

func TestUsageQueryGroupsRequestedModelsByPlan(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false}}`))
	}))
	defer upstream.Close()

	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"token","account_id":"account"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	service := newUsageServiceWithPlans([]planConfig{
		{Provider: "openai", Plan: "openai_plan_01", Models: []modelMapping{{LiteLLMName: "oai-gpt-5.6-sol"}, {LiteLLMName: "oai-gpt-5.6-luna"}}, UsageURL: upstream.URL + "/openai", AuthFile: authPath},
		{Provider: "openai", Plan: "zai_plan_01", Models: []modelMapping{{LiteLLMName: "glm-5.2"}}, UsageURL: upstream.URL + "/zai", AuthFile: authPath},
	}, time.Minute, upstream.Client())
	handler := newHandler("secret", service)

	request := httptest.NewRequest(http.MethodGet, "/v1/usage?m=oai-gpt-5.6-sol,glm-5.2,oai-gpt-5.6-luna", nil)
	request.Header.Set("X-Internal-API-Key", "secret")
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, request)

	var response apiUsageListEnvelope
	if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if result.Code != http.StatusOK || !response.IsSuccess || len(response.Data) != 2 {
		t.Fatalf("unexpected query response: status=%d body=%s", result.Code, result.Body.String())
	}
	if response.Data[0].PlanID != "openai_plan_01" || !reflect.DeepEqual(response.Data[0].RequestedModels, []string{"oai-gpt-5.6-sol", "oai-gpt-5.6-luna"}) {
		t.Fatalf("unexpected first plan result: %+v", response.Data[0])
	}
	if response.Data[1].PlanID != "zai_plan_01" || !reflect.DeepEqual(response.Data[1].RequestedModels, []string{"glm-5.2"}) {
		t.Fatalf("unexpected second plan result: %+v", response.Data[1])
	}
	if requests.Load() != 2 {
		t.Fatalf("expected one upstream request per plan, got %d", requests.Load())
	}
}

func TestUsageQueryValidationAndUnlinkedModels(t *testing.T) {
	t.Parallel()

	plan := planConfig{Provider: "openai", Plan: "openai_plan_01", Models: []modelMapping{{LiteLLMName: "known"}}, UsageURL: "https://example.com/usage", AuthFile: "/tokens/auth.json"}
	service := &usageService{
		plans: []planConfig{plan},
		caches: map[string]*cacheEntry{
			planCacheKey(plan): {value: usageResponse{PlanType: "pro", RetrievedAt: "2026-07-17T00:00:00Z"}, expires: time.Now().Add(time.Minute)},
		},
	}
	handler := newHandler("secret", service)

	for _, target := range []string{"/v1/usage?m=", "/v1/usage?m=known,", "/v1/usage?m=known,,other"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("X-Internal-API-Key", "secret")
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		if result.Code != http.StatusBadRequest {
			t.Fatalf("target %q: expected 400, got status=%d body=%s", target, result.Code, result.Body.String())
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/usage?m=known,unknown", nil)
	request.Header.Set("X-Internal-API-Key", "secret")
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, request)
	var response apiUsageListEnvelope
	if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if result.Code != http.StatusOK || len(response.Data) != 1 || !reflect.DeepEqual(response.Data[0].RequestedModels, []string{"known"}) {
		t.Fatalf("expected only the linked model, got status=%d body=%s", result.Code, result.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/usage?m=unknown,also-unknown", nil)
	request.Header.Set("X-Internal-API-Key", "secret")
	result = httptest.NewRecorder()
	handler.ServeHTTP(result, request)
	if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if result.Code != http.StatusOK || !response.IsSuccess || len(response.Data) != 0 {
		t.Fatalf("expected an empty successful result, got status=%d body=%s", result.Code, result.Body.String())
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

func TestLoadLiteLLMConfigGroupsAnchoredModelMetadata(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configYAML := `subscription_usage_plans:
	openai: &openai_plan
		provider: openai
		plan: openai_plan_01
		auth_file: /tokens/openai.json
		cache_ttl_seconds: 45
	zai: &zai_plan
		provider: zai
		plan: zai_plan_01
		auth_env: Z_AI_API_KEY
model_list:
	- model_name: oai-one
		model_info:
			subscription_usage: *openai_plan
	- model_name: oai-two
		model_info:
			subscription_usage: *openai_plan
	- model_name: glm-one
		model_info:
			subscription_usage: *zai_plan
	- model_name: embedding
		model_info:
			mode: embedding
`
	configYAML = strings.ReplaceAll(configYAML, "\t", "  ")
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := loadLiteLLMConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	plans, err := loadPlans(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].Provider != "openai" || plans[0].Plan != "openai_plan_01" || len(plans[0].Models) != 2 || plans[0].Models[1].LiteLLMName != "oai-two" || plans[0].AuthFile != "/tokens/openai.json" || plans[0].CacheTTLSeconds != 45 || plans[1].Provider != "zai" || plans[1].Models[0].LiteLLMName != "glm-one" || plans[1].AuthEnv != "Z_AI_API_KEY" {
		t.Fatalf("unexpected plans: %+v", plans)
	}
}

func TestMCPInitializeAndToolCall(t *testing.T) {
	t.Parallel()

	plan := planConfig{Provider: "openai", Plan: "openai_plan_01", Models: []modelMapping{{LiteLLMName: "oai-gpt-5.5"}}, UsageURL: "https://example.com/usage", AuthFile: "/tokens/auth.json"}
	service := &usageService{
		plans: []planConfig{plan},
		caches: map[string]*cacheEntry{
			planCacheKey(plan): {value: usageResponse{PlanType: "pro", RetrievedAt: "2026-07-13T00:00:00Z"}, expires: time.Now().Add(time.Minute)},
		},
	}
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

	callBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_model_usage","arguments":{"model_id":"oai-gpt-5.5"}}}`
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
