package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	"litellm-subscription-usage-sidecar/providers"
)

const (
	protocolVersion      = "2025-11-25"
	defaultLiteLLMConfig = "/data/litellm-config.yaml"
	defaultCacheTTL      = 60 * time.Second
	maxRequestedModels   = 50
)

var errUnknownModel = errors.New("unknown model")

type providersConfig struct {
	Plans []planConfig `json:"plans"`
}

type liteLLMConfig struct {
	ModelList []liteLLMModel `yaml:"model_list"`
}

type liteLLMModel struct {
	ModelName string           `yaml:"model_name"`
	ModelInfo liteLLMModelInfo `yaml:"model_info"`
}

type liteLLMModelInfo struct {
	SubscriptionUsage *subscriptionUsageConfig `yaml:"subscription_usage"`
}

type subscriptionUsageConfig struct {
	Provider        string `yaml:"provider"`
	Plan            string `yaml:"plan"`
	AuthFile        string `yaml:"auth_file,omitempty"`
	AuthEnv         string `yaml:"auth_env,omitempty"`
	CacheTTLSeconds int    `yaml:"cache_ttl_seconds,omitempty"`
}

type usageWindow = providers.UsageWindow
type usageLimit = providers.UsageLimit
type usageResponse = providers.UsageResponse

type usagePlan struct {
	Provider        string         `json:"provider"`
	Plan            string         `json:"plan"`
	Models          []modelMapping `json:"models"`
	RequestedModels []string       `json:"requested_models,omitempty"`
	UsagePath       string         `json:"usage_path"`
	PlanDetailsPath string         `json:"plan_details_path,omitempty"`
	Usage           usageResponse  `json:"usage"`
}

type usagePlansResponse []usagePlan

type apiUsageEntry struct {
	Type             string  `json:"type"`
	Name             string  `json:"name"`
	Period           string  `json:"period"`
	UsedPercent      float64 `json:"used_percent"`
	RemainingPercent float64 `json:"remaining_percent"`
	StartAt          string  `json:"start_at,omitempty"`
	ResetAt          string  `json:"reset_at,omitempty"`
}

type apiUsageData struct {
	Provider        string          `json:"provider"`
	PlanID          string          `json:"plan_id"`
	ExternalPlanID  string          `json:"external_plan_id,omitempty"`
	ModelID         string          `json:"model_id,omitempty"`
	RequestedModels []string        `json:"requested_models,omitempty"`
	IsActive        bool            `json:"is_active"`
	IsBlocked       bool            `json:"is_blocked"`
	Usage           []apiUsageEntry `json:"usage"`
}

type apiUsageEnvelope struct {
	IsSuccess   bool         `json:"is_success"`
	Message     string       `json:"message"`
	RetrievedAt string       `json:"retrieved_at"`
	Data        apiUsageData `json:"data"`
}

type apiUsageListEnvelope struct {
	IsSuccess   bool           `json:"is_success"`
	Message     string         `json:"message"`
	RetrievedAt string         `json:"retrieved_at"`
	Data        []apiUsageData `json:"data"`
}

type apiErrorEnvelope struct {
	IsSuccess   bool   `json:"is_success"`
	Message     string `json:"message"`
	RetrievedAt string `json:"retrieved_at"`
}

type cacheEntry struct {
	value   usageResponse
	expires time.Time
}

type modelMapping struct {
	LiteLLMName string `json:"litellm_name"`
}

type planConfig struct {
	Provider        string         `json:"provider"`
	Plan            string         `json:"plan"`
	Models          []modelMapping `json:"models"`
	AuthFile        string         `json:"auth_file,omitempty"`
	AuthEnv         string         `json:"auth_env,omitempty"`
	CacheTTLSeconds int            `json:"cache_ttl_seconds,omitempty"`

	UsageURL        string `json:"-"`
	PlanDetailsPath string `json:"-"`
}

type usageService struct {
	plans  []planConfig
	ttl    time.Duration
	client *http.Client
	mu     sync.Mutex
	caches map[string]*cacheEntry
	cache  *cacheEntry
}

func newUsageService(authFilePath, usageURL string, ttl time.Duration, client *http.Client) *usageService {
	return newUsageServiceWithPlans([]planConfig{{
		Provider: "openai", Plan: "openai_plan_01", Models: []modelMapping{{
			LiteLLMName: "oai-gpt-5.5",
		}},
		UsageURL: usageURL, AuthFile: authFilePath,
	}}, ttl, client)
}

func newUsageServiceWithPlans(plans []planConfig, ttl time.Duration, client *http.Client) *usageService {
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}
	return &usageService{plans: plans, ttl: ttl, client: client, caches: make(map[string]*cacheEntry)}
}

func (s *usageService) retrieve(ctx context.Context, modelIDs ...string) (usageResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	modelID := ""
	if len(modelIDs) > 0 && modelIDs[0] != "" {
		modelID = modelIDs[0]
	}
	plan, err := s.planForModel(modelID)
	if err != nil {
		return usageResponse{}, err
	}
	value, err := s.retrievePlanLocked(ctx, plan)
	if err != nil {
		return usageResponse{}, err
	}
	value.ModelID = modelID
	return value, nil
}

func (s *usageService) retrievePlanLocked(ctx context.Context, plan planConfig) (usageResponse, error) {
	if s.caches == nil {
		s.caches = make(map[string]*cacheEntry)
	}

	cacheKey := planCacheKey(plan)
	if cache := s.caches[cacheKey]; cache != nil && time.Now().Before(cache.expires) {
		return cache.value, nil
	}

	value, err := s.fetch(ctx, plan)
	if err != nil {
		return usageResponse{}, err
	}
	s.caches[cacheKey] = &cacheEntry{value: value, expires: time.Now().Add(s.cacheTTL(plan))}
	return value, nil
}

func (s *usageService) cacheTTL(plan planConfig) time.Duration {
	if plan.CacheTTLSeconds > 0 {
		return time.Duration(plan.CacheTTLSeconds) * time.Second
	}
	return s.ttl
}

func (s *usageService) planForModel(modelID string) (planConfig, error) {
	if modelID == "" {
		return planConfig{}, fmt.Errorf("%w: model ID is required", errUnknownModel)
	}
	for _, plan := range s.plans {
		for _, model := range plan.Models {
			if model.LiteLLMName == modelID {
				return plan, nil
			}
		}
	}
	return planConfig{}, fmt.Errorf("%w %q", errUnknownModel, modelID)
}

func (s *usageService) plansResponse(ctx context.Context) (usagePlansResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	plans := make([]usagePlan, 0, len(s.plans))
	for _, plan := range s.plans {
		models := append([]modelMapping(nil), plan.Models...)
		usage, err := s.retrievePlanLocked(ctx, plan)
		if err != nil {
			return nil, err
		}
		plans = append(plans, usagePlan{
			Provider: planProvider(plan), Plan: plan.Plan, Models: models,
			UsagePath: "/v1/usage/{model_id}", PlanDetailsPath: plan.PlanDetailsPath,
			Usage: usage,
		})
	}
	return usagePlansResponse(plans), nil
}

func (s *usageService) requestedModelsResponse(ctx context.Context, modelIDs []string) (usagePlansResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	planIndexes := make(map[string]int)
	plans := make([]usagePlan, 0, len(modelIDs))
	resolvedPlans := make([]planConfig, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		plan, err := s.planForModel(modelID)
		if err != nil {
			if errors.Is(err, errUnknownModel) {
				continue
			}
			return nil, err
		}
		key := planCacheKey(plan)
		if index, exists := planIndexes[key]; exists {
			plans[index].RequestedModels = append(plans[index].RequestedModels, modelID)
			continue
		}
		planIndexes[key] = len(plans)
		plans = append(plans, usagePlan{
			Provider: planProvider(plan), Plan: plan.Plan,
			RequestedModels: []string{modelID},
		})
		resolvedPlans = append(resolvedPlans, plan)
	}

	for index := range plans {
		usage, err := s.retrievePlanLocked(ctx, resolvedPlans[index])
		if err != nil {
			return nil, err
		}
		plans[index].Usage = usage
	}
	return usagePlansResponse(plans), nil
}

func parseRequestedModels(values []string) ([]string, error) {
	models := make([]string, 0)
	for _, value := range values {
		for _, model := range strings.Split(value, ",") {
			model = strings.TrimSpace(model)
			if model == "" {
				return nil, errors.New("query parameter m contains an empty model name")
			}
			models = append(models, model)
			if len(models) > maxRequestedModels {
				return nil, fmt.Errorf("query parameter m supports at most %d model names", maxRequestedModels)
			}
		}
	}
	if len(models) == 0 {
		return nil, errors.New("query parameter m requires at least one model name")
	}
	return models, nil
}

func planProvider(plan planConfig) string {
	return strings.ToLower(strings.TrimSpace(plan.Provider))
}

func planCacheKey(plan planConfig) string {
	return strings.Join([]string{planProvider(plan), plan.Plan, plan.UsageURL, plan.AuthFile, plan.AuthEnv}, "\x00")
}

func (s *usageService) fetch(ctx context.Context, plan planConfig) (usageResponse, error) {
	return providers.Fetch(ctx, s.client, providerPlan(plan))
}

func providerPlan(plan planConfig) providers.Plan {
	return providers.Plan{
		Provider: planProvider(plan), Plan: plan.Plan, UsageURL: plan.UsageURL,
		AuthFile: plan.AuthFile, AuthEnv: plan.AuthEnv,
	}
}

func loadLiteLLMConfig(path string) (providersConfig, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return providersConfig{}, fmt.Errorf("read LiteLLM config: %w", err)
	}

	var source liteLLMConfig
	if err := yaml.Unmarshal(contents, &source); err != nil {
		return providersConfig{}, fmt.Errorf("parse LiteLLM config: %w", err)
	}

	config := providersConfig{Plans: make([]planConfig, 0)}
	planIndexes := make(map[string]int)
	for _, model := range source.ModelList {
		metadata := model.ModelInfo.SubscriptionUsage
		if metadata == nil {
			continue
		}
		modelName := strings.TrimSpace(model.ModelName)
		provider := strings.ToLower(strings.TrimSpace(metadata.Provider))
		planName := strings.TrimSpace(metadata.Plan)
		if modelName == "" {
			return providersConfig{}, errors.New("subscription-enabled model requires model_name")
		}
		key := provider + "\x00" + planName
		if index, exists := planIndexes[key]; exists {
			plan := &config.Plans[index]
			if plan.AuthFile != metadata.AuthFile || plan.AuthEnv != metadata.AuthEnv || plan.CacheTTLSeconds != metadata.CacheTTLSeconds {
				return providersConfig{}, fmt.Errorf("models on plan %q/%q must use identical subscription metadata", provider, planName)
			}
			plan.Models = append(plan.Models, modelMapping{LiteLLMName: modelName})
			continue
		}
		planIndexes[key] = len(config.Plans)
		config.Plans = append(config.Plans, planConfig{
			Provider:        provider,
			Plan:            planName,
			Models:          []modelMapping{{LiteLLMName: modelName}},
			AuthFile:        metadata.AuthFile,
			AuthEnv:         metadata.AuthEnv,
			CacheTTLSeconds: metadata.CacheTTLSeconds,
		})
	}
	if err := validateProvidersConfig(config); err != nil {
		return providersConfig{}, fmt.Errorf("validate LiteLLM subscription metadata: %w", err)
	}
	return config, nil
}

func validateProvidersConfig(config providersConfig) error {
	if len(config.Plans) == 0 {
		return errors.New("at least one plan is required")
	}
	modelOwners := make(map[string]string)
	for _, plan := range config.Plans {
		provider := planProvider(plan)
		if provider == "" {
			return errors.New("each plan requires provider")
		}
		if _, ok := providers.DefinitionFor(provider); !ok {
			return fmt.Errorf("provider %q has no built-in definition", provider)
		}
		if plan.Plan == "" {
			return fmt.Errorf("plan for provider %q requires plan", provider)
		}
		if (plan.AuthFile == "") == (plan.AuthEnv == "") {
			return fmt.Errorf("plan %q requires exactly one of auth_file or auth_env", plan.Plan)
		}
		if plan.CacheTTLSeconds < 0 {
			return fmt.Errorf("plan %q cache_ttl_seconds cannot be negative", plan.Plan)
		}
		if len(plan.Models) == 0 {
			return fmt.Errorf("plan %q must list at least one model", plan.Plan)
		}
		for _, model := range plan.Models {
			if model.LiteLLMName == "" {
				return fmt.Errorf("plan %q requires litellm_name for every model", plan.Plan)
			}
			if owner, exists := modelOwners[model.LiteLLMName]; exists {
				return fmt.Errorf("model %q is assigned to plans %q and %q", model.LiteLLMName, owner, plan.Plan)
			}
			modelOwners[model.LiteLLMName] = plan.Plan
		}
	}
	return nil
}

func loadPlans(config providersConfig) ([]planConfig, error) {
	if err := validateProvidersConfig(config); err != nil {
		return nil, err
	}
	for index := range config.Plans {
		provider := planProvider(config.Plans[index])
		definition, ok := providers.DefinitionFor(provider)
		if !ok {
			return nil, fmt.Errorf("provider %q has no built-in definition", provider)
		}
		config.Plans[index].Provider = provider
		config.Plans[index].UsageURL = definition.UsageURL
		config.Plans[index].PlanDetailsPath = definition.PlanDetailsPath
	}
	if err := validatePlans(config.Plans); err != nil {
		return nil, err
	}
	return config.Plans, nil
}

func validatePlans(plans []planConfig) error {
	if len(plans) == 0 {
		return errors.New("at least one plan is required")
	}

	planKeys := make(map[string]struct{}, len(plans))
	modelOwners := make(map[string]string)
	for _, plan := range plans {
		if plan.Provider == "" {
			return errors.New("each plan requires provider")
		}
		if _, ok := providers.DefinitionFor(planProvider(plan)); !ok {
			return fmt.Errorf("provider %q has no built-in definition", plan.Provider)
		}
		if plan.Plan == "" {
			return fmt.Errorf("plan for provider %q requires plan", plan.Provider)
		}
		if (plan.AuthFile == "") == (plan.AuthEnv == "") {
			return fmt.Errorf("plan %q requires exactly one of auth_file or auth_env", plan.Plan)
		}
		if plan.CacheTTLSeconds < 0 {
			return fmt.Errorf("plan %q cache_ttl_seconds cannot be negative", plan.Plan)
		}
		planKey := planCacheKey(plan)
		if _, exists := planKeys[planKey]; exists {
			return fmt.Errorf("duplicate provider plan source %q/%q", plan.Provider, plan.Plan)
		}
		planKeys[planKey] = struct{}{}
		parsedURL, err := url.Parse(plan.UsageURL)
		if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
			return fmt.Errorf("plan %q has invalid usage_url", plan.Plan)
		}
		if len(plan.Models) == 0 {
			return fmt.Errorf("plan %q must list at least one model", plan.Plan)
		}
		for _, model := range plan.Models {
			if model.LiteLLMName == "" {
				return fmt.Errorf("plan %q requires litellm_name for every model", plan.Plan)
			}
			if owner, exists := modelOwners[model.LiteLLMName]; exists {
				return fmt.Errorf("model %q is assigned to plans %q and %q", model.LiteLLMName, owner, plan.Plan)
			}
			modelOwners[model.LiteLLMName] = plan.Plan
		}
	}
	return nil
}

func normalizeUsageData(usage usageResponse) apiUsageData {
	entries := make([]apiUsageEntry, 0, 2+(2*len(usage.AdditionalLimits)))
	entries = appendWindowEntries(entries, "primary", usage.PlanType, usage.Primary, usage.Secondary)
	for _, additional := range usage.AdditionalLimits {
		typeName := additional.MeteredFeature
		if typeName == "" {
			typeName = additional.Name
		}
		if typeName == "" {
			typeName = "additional"
		}
		name := additional.Name
		if name == "" {
			name = usage.PlanType
		}
		entries = appendWindowEntries(entries, typeName, name, additional.Primary, additional.Secondary)
	}

	planName := usage.UsagePlanName
	if planName == "" {
		planName = usage.PlanType
	}

	return apiUsageData{
		Provider:  usage.Provider,
		PlanID:    planName,
		ModelID:   usage.ModelID,
		IsActive:  usage.Allowed,
		IsBlocked: !usage.Allowed || usage.LimitReached,
		Usage:     entries,
	}
}

func appendWindowEntries(entries []apiUsageEntry, typeName, name string, primary *usageWindow, secondary *usageWindow) []apiUsageEntry {
	if primary != nil {
		entries = append(entries, newUsageEntry(typeName, name, "5-hourly", 5*time.Hour, primary))
	}
	if secondary != nil {
		entries = append(entries, newUsageEntry(typeName, name, "weekly", 7*24*time.Hour, secondary))
	}
	return entries
}

func newUsageEntry(typeName, name, period string, duration time.Duration, window *usageWindow) apiUsageEntry {
	entry := apiUsageEntry{
		Type:             typeName,
		Name:             name,
		Period:           period,
		UsedPercent:      window.UsedPercent,
		RemainingPercent: window.RemainingPercent,
		ResetAt:          window.ResetsAt,
	}
	if duration <= 0 || window.ResetsAt == "" {
		return entry
	}
	resetAt, err := time.Parse(time.RFC3339, window.ResetsAt)
	if err != nil {
		return entry
	}
	entry.StartAt = resetAt.Add(-duration).UTC().Format(time.RFC3339)
	return entry
}

func usageEnvelope(usage usageResponse) apiUsageEnvelope {
	return apiUsageEnvelope{
		IsSuccess:   true,
		Message:     "OK",
		RetrievedAt: usage.RetrievedAt,
		Data:        normalizeUsageData(usage),
	}
}

func usageListEnvelope(plans usagePlansResponse) apiUsageListEnvelope {
	items := make([]apiUsageData, 0, len(plans))
	retrievedAt := time.Now().UTC().Format(time.RFC3339)
	for _, plan := range plans {
		item := normalizeUsageData(plan.Usage)
		if item.Provider == "" {
			item.Provider = plan.Provider
		}
		if item.PlanID == "" {
			item.PlanID = plan.Plan
		}
		item.RequestedModels = plan.RequestedModels
		items = append(items, item)
		if plan.Usage.RetrievedAt != "" {
			retrievedAt = plan.Usage.RetrievedAt
		}
	}
	return apiUsageListEnvelope{IsSuccess: true, Message: "OK", RetrievedAt: retrievedAt, Data: items}
}

func errorEnvelope(message string) apiErrorEnvelope {
	return apiErrorEnvelope{IsSuccess: false, Message: message, RetrievedAt: time.Now().UTC().Format(time.RFC3339)}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type toolCallParams struct {
	Name      string `json:"name"`
	Arguments struct {
		ModelID string `json:"model_id"`
	} `json:"arguments"`
}

func newHandler(internalKey string, service *usageService) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/usage/{model_id...}", authenticate(internalKey, func(w http.ResponseWriter, r *http.Request) {
		usage, err := service.retrieve(r.Context(), r.PathValue("model_id"))
		if err != nil {
			if errors.Is(err, errUnknownModel) {
				writeJSON(w, http.StatusNotFound, errorEnvelope(err.Error()))
				return
			}
			slog.Error("usage retrieval failed", "error", err)
			writeJSON(w, http.StatusBadGateway, errorEnvelope("usage retrieval failed: "+err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, usageEnvelope(usage))
	}))
	mux.HandleFunc("GET /v1/usage", authenticate(internalKey, func(w http.ResponseWriter, r *http.Request) {
		var plans usagePlansResponse
		var err error
		if values, requested := r.URL.Query()["m"]; requested {
			models, parseErr := parseRequestedModels(values)
			if parseErr != nil {
				writeJSON(w, http.StatusBadRequest, errorEnvelope(parseErr.Error()))
				return
			}
			plans, err = service.requestedModelsResponse(r.Context(), models)
		} else {
			plans, err = service.plansResponse(r.Context())
		}
		if err != nil {
			if errors.Is(err, errUnknownModel) {
				writeJSON(w, http.StatusNotFound, errorEnvelope(err.Error()))
				return
			}
			slog.Error("plan usage retrieval failed", "error", err)
			writeJSON(w, http.StatusBadGateway, errorEnvelope("usage retrieval failed: "+err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, usageListEnvelope(plans))
	}))
	mux.HandleFunc("POST /mcp", authenticate(internalKey, func(w http.ResponseWriter, r *http.Request) {
		handleMCP(w, r, service)
	}))
	return mux
}

func authenticate(key string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("X-Internal-API-Key")
		if key == "" || len(provided) != len(key) || subtle.ConstantTimeCompare([]byte(provided), []byte(key)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func handleMCP(w http.ResponseWriter, r *http.Request, service *usageService) {
	if r.Header.Get("Content-Type") != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, errorEnvelope("Content-Type must be application/json"))
		return
	}
	var request rpcRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := decoder.Decode(&request); err != nil {
		writeRPC(w, nil, nil, &rpcError{Code: -32700, Message: "Parse error"})
		return
	}

	switch request.Method {
	case "initialize":
		writeRPC(w, request.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]string{"name": "litellm-subscription-usage-sidecar", "version": "1.2.0"},
		}, nil)
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "ping":
		writeRPC(w, request.ID, map[string]any{}, nil)
	case "tools/list":
		writeRPC(w, request.ID, map[string]any{"tools": []any{map[string]any{
			"name":        "get_model_usage",
			"description": "Get current provider allowance, remaining percentage, and reset times for a configured LiteLLM model.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"model_id": map[string]any{"type": "string", "description": "Configured LiteLLM model name"},
				},
				"required": []string{"model_id"}, "additionalProperties": false,
			},
		}}}, nil)
	case "tools/call":
		var params toolCallParams
		if err := json.Unmarshal(request.Params, &params); err != nil || params.Name != "get_model_usage" || params.Arguments.ModelID == "" {
			writeRPC(w, request.ID, nil, &rpcError{Code: -32602, Message: "Invalid params"})
			return
		}
		usage, err := service.retrieve(r.Context(), params.Arguments.ModelID)
		if err != nil {
			slog.Error("MCP usage retrieval failed", "error", err)
			writeRPC(w, request.ID, map[string]any{"isError": true, "content": []any{map[string]string{"type": "text", "text": "Usage retrieval failed"}}}, nil)
			return
		}
		payload := usageEnvelope(usage)
		structured, _ := json.Marshal(payload)
		writeRPC(w, request.ID, map[string]any{
			"content":           []any{map[string]string{"type": "text", "text": summarizeUsage(usage)}},
			"structuredContent": json.RawMessage(structured),
			"isError":           false,
		}, nil)
	default:
		if len(request.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeRPC(w, request.ID, nil, &rpcError{Code: -32601, Message: "Method not found"})
	}
}

func summarizeUsage(usage usageResponse) string {
	if usage.Primary == nil {
		return fmt.Sprintf("Usage for %s on the %s plan is available, but no primary allowance window was returned.", usage.ModelID, usage.PlanType)
	}
	reset := ""
	if usage.Primary.ResetsAt != "" {
		reset = "; resets at " + usage.Primary.ResetsAt
	}
	return fmt.Sprintf("%s (%s): %.0f%% remaining (%.0f%% used)%s.", usage.ModelID, usage.PlanType, usage.Primary.RemainingPercent, usage.Primary.UsedPercent, reset)
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *rpcError) {
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("response encoding failed", "error", err)
	}
}

func envDuration(name string, fallback time.Duration) time.Duration {
	seconds, err := strconv.Atoi(os.Getenv(name))
	if err != nil || seconds < 1 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		response, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://127.0.0.1:8080/health")
		if err != nil || response.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		_ = response.Body.Close()
		return
	}

	internalKey := os.Getenv("INTERNAL_API_KEY")
	if internalKey == "" {
		slog.Error("INTERNAL_API_KEY is required")
		os.Exit(1)
	}
	configPath := os.Getenv("LITELLM_CONFIG_FILE")
	if configPath == "" {
		configPath = defaultLiteLLMConfig
	}
	config, err := loadLiteLLMConfig(configPath)
	if err != nil {
		slog.Error("invalid provider configuration", "error", err)
		os.Exit(1)
	}
	plans, err := loadPlans(config)
	if err != nil {
		slog.Error("invalid provider plan configuration", "error", err)
		os.Exit(1)
	}

	client := &http.Client{Timeout: envDuration("UPSTREAM_TIMEOUT_SECONDS", 10*time.Second)}
	service := newUsageServiceWithPlans(plans, envDuration("CACHE_TTL_SECONDS", defaultCacheTTL), client)
	server := &http.Server{
		Addr:              ":8080",
		Handler:           newHandler(internalKey, service),
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	slog.Info("codex usage sidecar listening", "address", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}
