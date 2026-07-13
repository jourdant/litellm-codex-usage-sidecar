package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	first, err := svc.retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first.Primary == nil || first.Primary.RemainingPercent != 64 || second.PlanType != "pro" {
		t.Fatalf("unexpected result: calls=%d first=%+v second=%+v", calls, first, second)
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
