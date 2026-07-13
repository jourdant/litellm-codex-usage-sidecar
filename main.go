package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	protocolVersion           = "2025-11-25"
	defaultProviderConfigFile = "/config/providers.json"
	defaultCacheTTL           = 60 * time.Second
)

var errUnknownModel = errors.New("unknown model")

type providersConfig struct {
	Plans []planConfig `json:"plans"`
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

type usageWindow struct {
	UsedPercent      float64 `json:"used_percent"`
	RemainingPercent float64 `json:"remaining_percent"`
	ResetsAt         string  `json:"resets_at,omitempty"`
}

type usageLimit struct {
	Name           string       `json:"name"`
	MeteredFeature string       `json:"metered_feature,omitempty"`
	Allowed        bool         `json:"allowed"`
	LimitReached   bool         `json:"limit_reached"`
	Primary        *usageWindow `json:"primary,omitempty"`
	Secondary      *usageWindow `json:"secondary,omitempty"`
}

type usageResponse struct {
	ModelID          string       `json:"model_id,omitempty"`
	Provider         string       `json:"provider,omitempty"`
	UsagePlanName    string       `json:"usage_plan_name,omitempty"`
	PlanType         string       `json:"plan_type"`
	Allowed          bool         `json:"allowed"`
	LimitReached     bool         `json:"limit_reached"`
	Primary          *usageWindow `json:"primary,omitempty"`
	Secondary        *usageWindow `json:"secondary,omitempty"`
	AdditionalLimits []usageLimit `json:"additional_limits,omitempty"`
	RetrievedAt      string       `json:"retrieved_at"`
}

type usagePlan struct {
	Provider        string         `json:"provider"`
	Plan            string         `json:"plan"`
	Models          []modelMapping `json:"models"`
	UsagePath       string         `json:"usage_path"`
	PlanDetailsPath string         `json:"plan_details_path,omitempty"`
	Usage           usageResponse  `json:"usage"`
}

type usagePlansResponse []usagePlan

type cacheEntry struct {
	value   usageResponse
	expires time.Time
}

type modelMapping struct {
	LiteLLMName string `json:"litellm_name"`
}

type planConfig struct {
	Provider string         `json:"provider"`
	Plan     string         `json:"plan"`
	Models   []modelMapping `json:"models"`
	AuthFile string         `json:"auth_file"`

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
	s.caches[cacheKey] = &cacheEntry{value: value, expires: time.Now().Add(s.ttl)}
	return value, nil
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

func planProvider(plan planConfig) string {
	return strings.ToLower(strings.TrimSpace(plan.Provider))
}

func planCacheKey(plan planConfig) string {
	return strings.Join([]string{planProvider(plan), plan.Plan, plan.UsageURL, plan.AuthFile}, "\x00")
}

func (s *usageService) fetch(ctx context.Context, plan planConfig) (usageResponse, error) {
	auth, err := readAuthFile(plan.AuthFile)
	if err != nil {
		return usageResponse{}, err
	}
	provider, ok := providerAdapterFor(plan.Provider)
	if !ok {
		return usageResponse{}, fmt.Errorf("provider %q has no built-in adapter", plan.Provider)
	}
	return provider.fetch(ctx, s.client, plan, auth)
}

func loadProviderConfig(path string) (providersConfig, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return providersConfig{}, fmt.Errorf("read provider config: %w", err)
	}

	var config providersConfig
	if err := json.Unmarshal(contents, &config); err != nil {
		return providersConfig{}, fmt.Errorf("parse provider config: %w", err)
	}
	if err := validateProvidersConfig(config); err != nil {
		return providersConfig{}, fmt.Errorf("validate provider config: %w", err)
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
		if _, ok := providerAdapterFor(provider); !ok {
			return fmt.Errorf("provider %q has no built-in definition", provider)
		}
		if plan.Plan == "" {
			return fmt.Errorf("plan for provider %q requires plan", provider)
		}
		if plan.AuthFile == "" {
			return fmt.Errorf("plan %q requires auth_file", plan.Plan)
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
		adapter, ok := providerAdapterFor(provider)
		if !ok {
			return nil, fmt.Errorf("provider %q has no built-in definition", provider)
		}
		definition := adapter.definition()
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
		if _, ok := providerAdapterFor(planProvider(plan)); !ok {
			return fmt.Errorf("provider %q has no built-in definition", plan.Provider)
		}
		if plan.Plan == "" {
			return fmt.Errorf("plan for provider %q requires plan", plan.Provider)
		}
		if plan.AuthFile == "" {
			return fmt.Errorf("plan %q requires auth_file because provider %q has no built-in auth file", plan.Plan, plan.Provider)
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

func normalizeUsage(upstream upstreamUsage, retrievedAt time.Time) usageResponse {
	additional := make([]usageLimit, 0, len(upstream.AdditionalRateLimits))
	for _, limit := range upstream.AdditionalRateLimits {
		additional = append(additional, usageLimit{
			Name: limit.LimitName, MeteredFeature: limit.MeteredFeature,
			Allowed: limit.RateLimit.Allowed, LimitReached: limit.RateLimit.LimitReached,
			Primary: normalizeWindow(limit.RateLimit.PrimaryWindow), Secondary: normalizeWindow(limit.RateLimit.SecondaryWindow),
		})
	}
	return usageResponse{
		PlanType: upstream.PlanType, Allowed: upstream.RateLimit.Allowed, LimitReached: upstream.RateLimit.LimitReached,
		Primary: normalizeWindow(upstream.RateLimit.PrimaryWindow), Secondary: normalizeWindow(upstream.RateLimit.SecondaryWindow),
		AdditionalLimits: additional, RetrievedAt: retrievedAt.Format(time.RFC3339),
	}
}

func normalizeWindow(window *upstreamWindow) *usageWindow {
	if window == nil {
		return nil
	}
	used := math.Max(0, math.Min(100, window.UsedPercent))
	reset := ""
	if window.ResetAt > 0 {
		reset = time.Unix(window.ResetAt, 0).UTC().Format(time.RFC3339)
	}
	return &usageWindow{UsedPercent: used, RemainingPercent: 100 - used, ResetsAt: reset}
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
	Name string `json:"name"`
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
				writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
				return
			}
			slog.Error("usage retrieval failed", "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "usage retrieval failed"})
			return
		}
		writeJSON(w, http.StatusOK, usage)
	}))
	mux.HandleFunc("GET /v1/usage", authenticate(internalKey, func(w http.ResponseWriter, r *http.Request) {
		plans, err := service.plansResponse(r.Context())
		if err != nil {
			slog.Error("plan usage retrieval failed", "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "usage retrieval failed"})
			return
		}
		writeJSON(w, http.StatusOK, plans)
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
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
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
			"serverInfo":      map[string]string{"name": "codex-usage", "version": "1.0.0"},
		}, nil)
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "ping":
		writeRPC(w, request.ID, map[string]any{}, nil)
	case "tools/list":
		writeRPC(w, request.ID, map[string]any{"tools": []any{map[string]any{
			"name":        "get_codex_usage",
			"description": "Get the shared ChatGPT account's current Codex allowance, remaining percentage, and reset times.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		}}}, nil)
	case "tools/call":
		var params toolCallParams
		if err := json.Unmarshal(request.Params, &params); err != nil || params.Name != "get_codex_usage" {
			writeRPC(w, request.ID, nil, &rpcError{Code: -32602, Message: "Invalid params"})
			return
		}
		usage, err := service.retrieve(r.Context(), "")
		if err != nil {
			slog.Error("MCP usage retrieval failed", "error", err)
			writeRPC(w, request.ID, map[string]any{"isError": true, "content": []any{map[string]string{"type": "text", "text": "Usage retrieval failed"}}}, nil)
			return
		}
		structured, _ := json.Marshal(usage)
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
		return fmt.Sprintf("Codex usage for the %s plan is available, but no primary allowance window was returned.", usage.PlanType)
	}
	reset := ""
	if usage.Primary.ResetsAt != "" {
		reset = "; resets at " + usage.Primary.ResetsAt
	}
	return fmt.Sprintf("Codex %s plan: %.0f%% remaining (%.0f%% used)%s.", usage.PlanType, usage.Primary.RemainingPercent, usage.Primary.UsedPercent, reset)
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
	configPath := os.Getenv("PROVIDER_CONFIG_FILE")
	if configPath == "" {
		configPath = defaultProviderConfigFile
	}
	config, err := loadProviderConfig(configPath)
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
