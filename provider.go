package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

type providerDefinition struct {
	UsageURL        string
	PlanDetailsPath string
}

type providerAdapter interface {
	definition() providerDefinition
	fetch(context.Context, *http.Client, planConfig, authFile) (usageResponse, error)
}

func providerAdapterFor(provider string) (providerAdapter, bool) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai":
		return openaiProvider{}, true
	case "zai":
		return zaiProvider{}, true
	default:
		return nil, false
	}
}

func readAuthFile(path string) (authFile, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return authFile{}, fmt.Errorf("read auth file: %w", err)
	}

	var auth authFile
	if err := json.Unmarshal(contents, &auth); err != nil {
		return authFile{}, fmt.Errorf("parse auth file: %w", err)
	}
	if strings.TrimSpace(auth.AccessToken) == "" {
		return authFile{}, fmt.Errorf("auth file is missing access_token")
	}
	return auth, nil
}

func providerGET(ctx context.Context, client *http.Client, endpoint, authorization string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create provider request: %w", err)
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex-usage-sidecar/1")
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request provider usage: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("provider endpoint returned HTTP %d", response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read provider response: %w", err)
	}
	return body, nil
}
