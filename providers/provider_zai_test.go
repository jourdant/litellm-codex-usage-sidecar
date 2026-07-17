package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestZAIProviderMapsQuotaWindows(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "zai-token" {
			t.Fatalf("unexpected z.ai authorization: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":15,"nextResetTime":1770648402389},{"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":40,"nextResetTime":1771300000000},{"type":"TIME_LIMIT","currentValue":10,"usage":100}]}}`))
	}))
	defer upstream.Close()

	authPath := filepath.Join(t.TempDir(), "zai-auth.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"zai-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	usage, err := Fetch(context.Background(), upstream.Client(), Plan{Provider: "zai", Plan: "zai_plan_01", UsageURL: upstream.URL, AuthFile: authPath})
	if err != nil {
		t.Fatal(err)
	}
	if usage.Provider != "zai" || usage.Primary == nil || usage.Primary.UsedPercent != 15 || usage.Secondary == nil || usage.Secondary.UsedPercent != 40 {
		t.Fatalf("unexpected z.ai usage: %+v", usage)
	}
}
