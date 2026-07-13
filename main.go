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
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	defaultAuthFile = "/tokens/auth.json"
	defaultUsageURL = "https://chatgpt.com/backend-api/wham/usage"
	protocolVersion = "2025-11-25"
)

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
	PlanType         string       `json:"plan_type"`
	Allowed          bool         `json:"allowed"`
	LimitReached     bool         `json:"limit_reached"`
	Primary          *usageWindow `json:"primary,omitempty"`
	Secondary        *usageWindow `json:"secondary,omitempty"`
	AdditionalLimits []usageLimit `json:"additional_limits,omitempty"`
	RetrievedAt      string       `json:"retrieved_at"`
}

type cacheEntry struct {
	value   usageResponse
	expires time.Time
}

type usageService struct {
	authFile string
	usageURL string
	ttl      time.Duration
	client   *http.Client
	mu       sync.Mutex
	cache    *cacheEntry
}

func newUsageService(authFilePath, usageURL string, ttl time.Duration, client *http.Client) *usageService {
	return &usageService{authFile: authFilePath, usageURL: usageURL, ttl: ttl, client: client}
}

func (s *usageService) retrieve(ctx context.Context) (usageResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cache != nil && time.Now().Before(s.cache.expires) {
		return s.cache.value, nil
	}

	value, err := s.fetch(ctx)
	if err != nil {
		return usageResponse{}, err
	}
	s.cache = &cacheEntry{value: value, expires: time.Now().Add(s.ttl)}
	return value, nil
}

func (s *usageService) fetch(ctx context.Context) (usageResponse, error) {
	contents, err := os.ReadFile(s.authFile)
	if err != nil {
		return usageResponse{}, fmt.Errorf("read auth file: %w", err)
	}

	var auth authFile
	if err := json.Unmarshal(contents, &auth); err != nil {
		return usageResponse{}, fmt.Errorf("parse auth file: %w", err)
	}
	if auth.AccessToken == "" || auth.AccountID == "" {
		return usageResponse{}, errors.New("auth file is missing access_token or account_id")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.usageURL, nil)
	if err != nil {
		return usageResponse{}, fmt.Errorf("create usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+auth.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", auth.AccountID)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex-usage-sidecar/1")

	response, err := s.client.Do(req)
	if err != nil {
		return usageResponse{}, fmt.Errorf("request usage: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return usageResponse{}, fmt.Errorf("usage endpoint returned HTTP %d", response.StatusCode)
	}

	var upstream upstreamUsage
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&upstream); err != nil {
		return usageResponse{}, fmt.Errorf("decode usage response: %w", err)
	}
	return normalizeUsage(upstream, time.Now().UTC()), nil
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
	mux.HandleFunc("GET /v1/usage", authenticate(internalKey, func(w http.ResponseWriter, r *http.Request) {
		usage, err := service.retrieve(r.Context())
		if err != nil {
			slog.Error("usage retrieval failed", "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "usage retrieval failed"})
			return
		}
		writeJSON(w, http.StatusOK, usage)
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
		usage, err := service.retrieve(r.Context())
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
	authPath := os.Getenv("CHATGPT_AUTH_FILE")
	if authPath == "" {
		authPath = defaultAuthFile
	}
	usageURL := os.Getenv("CHATGPT_USAGE_URL")
	if usageURL == "" {
		usageURL = defaultUsageURL
	}

	client := &http.Client{Timeout: envDuration("UPSTREAM_TIMEOUT_SECONDS", 10*time.Second)}
	service := newUsageService(authPath, usageURL, envDuration("CACHE_TTL_SECONDS", 30*time.Second), client)
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
