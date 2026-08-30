package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/one-search/one-search/backend/internal/model"
)

func TestQueryTavilyQuota(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/usage" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tavily-key" {
			t.Fatalf("Authorization = %q", got)
		}
		writeQuotaJSON(t, w, map[string]interface{}{
			"key": map[string]interface{}{
				"usage":        150,
				"limit":        1000,
				"search_usage": 100,
			},
			"account": map[string]interface{}{
				"current_plan": "Bootstrap",
				"plan_usage":   500,
				"plan_limit":   15000,
			},
		})
	}))
	defer server.Close()
	config := defaultQuotaQueryConfig()
	config.tavilyUsageURL = server.URL + "/usage"

	result, err := queryOfficialQuota(context.Background(), model.APIKey{ProviderName: model.ProviderTavily, Alias: "tavily", Value: "tavily-key"}, model.ProviderKeyQuotaRequest{}, config)
	if err != nil {
		t.Fatalf("QueryOfficialQuota returned error: %v", err)
	}
	if !result.Supported || result.Status != "success" || result.Source != model.QuotaSourceOfficial || result.Confidence != model.QuotaConfidenceExact || result.Unit != "credits" || result.Balance == nil || *result.Balance != 850 || result.TotalQuantity == nil || *result.TotalQuantity != 150 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestQueryFirecrawlQuota(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/team/credit-usage" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer firecrawl-key" {
			t.Fatalf("Authorization = %q", got)
		}
		writeQuotaJSON(t, w, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"remainingCredits":   1000,
				"planCredits":        500000,
				"billingPeriodStart": "2025-01-01T00:00:00Z",
				"billingPeriodEnd":   "2025-01-31T23:59:59Z",
			},
		})
	}))
	defer server.Close()
	config := defaultQuotaQueryConfig()
	config.firecrawlCreditUsageURL = server.URL + "/v2/team/credit-usage"

	result, err := queryOfficialQuota(context.Background(), model.APIKey{ProviderName: model.ProviderFirecrawl, Alias: "firecrawl", Value: "firecrawl-key"}, model.ProviderKeyQuotaRequest{}, config)
	if err != nil {
		t.Fatalf("QueryOfficialQuota returned error: %v", err)
	}
	if !result.Supported || result.Status != "success" || result.Source != model.QuotaSourceOfficial || result.Confidence != model.QuotaConfidenceExact || result.Unit != "credits" || result.Balance == nil || *result.Balance != 1000 || result.TotalQuantity == nil || *result.TotalQuantity != 499000 || result.Period["start"] == "" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestQuerySerperQuota(t *testing.T) {
	result, err := QueryOfficialQuota(context.Background(), model.APIKey{ProviderName: model.ProviderSerper, Alias: "serper", Value: "serper-key", MonthlyCredits: 10, UsageCreditsTotal: 100, UsageRequestsTotal: 120}, model.ProviderKeyQuotaRequest{})
	if err != nil {
		t.Fatalf("QueryOfficialQuota returned error: %v", err)
	}
	if !result.Supported || result.Status != "success" || result.Source != model.QuotaSourceLocalMeter || result.Confidence != model.QuotaConfidenceEstimated || result.Unit != "credits" || result.Balance == nil || *result.Balance != 2400 || result.TotalQuantity == nil || *result.TotalQuantity != 100 || !strings.Contains(result.Message, "2500 credits") {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestQueryExaQuotaFallsBackToLocalMeterWithoutManagementKey(t *testing.T) {
	result, err := QueryOfficialQuota(context.Background(), model.APIKey{ProviderName: model.ProviderExa, Alias: "exa", Value: "exa-key", UsageRequestsTotal: 12, UsageCostUSDTotal: 0.084}, model.ProviderKeyQuotaRequest{})
	if err != nil {
		t.Fatalf("QueryOfficialQuota returned error: %v", err)
	}
	if !result.Supported || result.Status != "success" || result.Source != model.QuotaSourceLocalMeter || result.Confidence != model.QuotaConfidenceEstimated || result.TotalCostUSD == nil || *result.TotalCostUSD != 0.084 || result.TotalQuantity == nil || *result.TotalQuantity != 12 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestQueryJinaQuotaFallsBackToLocalTokensWhenBalanceFormatIsMissing(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("Jina account response without a balance line"))
	}))
	defer server.Close()
	config := defaultQuotaQueryConfig()
	config.jinaQuotaURL = server.URL
	result, err := queryOfficialQuota(context.Background(), model.APIKey{ProviderName: model.ProviderJina, Alias: "jina", Value: "jina-key", UsageTokensTotal: 321, UsageRequestsTotal: 4}, model.ProviderKeyQuotaRequest{}, config)
	if err != nil {
		t.Fatalf("queryOfficialQuota returned error: %v", err)
	}
	if result.Source != model.QuotaSourceLocalMeter || result.Unit != "tokens_used" || result.Balance != nil || result.TotalQuantity == nil || *result.TotalQuantity != 321 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestQueryOfficialQuotaUsesBoundedTimeout(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		writeQuotaJSON(t, w, map[string]interface{}{"data": map[string]interface{}{"attributes": map[string]interface{}{"balance": 100}}})
	}))
	defer server.Close()
	config := defaultQuotaQueryConfig()
	config.youQuotaURL = server.URL
	config.requestTimeout = 20 * time.Millisecond

	_, err := queryOfficialQuota(context.Background(), model.APIKey{ProviderName: model.ProviderYou, Alias: "you", Value: "you-key"}, model.ProviderKeyQuotaRequest{}, config)
	if err == nil {
		t.Fatalf("QueryOfficialQuota returned nil error")
	}
}

func TestQueryBraveQuota(t *testing.T) {
	balance := 14523.0
	used := 477.0
	checkedAt := time.Now().Add(-time.Minute)
	result, err := QueryOfficialQuota(context.Background(), model.APIKey{ProviderName: model.ProviderBrave, Alias: "brave", OfficialQuotaSource: model.QuotaSourceResponseHeader, OfficialQuotaConfidence: model.QuotaConfidenceBestEffort, OfficialQuotaUnit: "requests", OfficialQuotaBalance: &balance, OfficialQuotaTotalQuantity: &used, OfficialQuotaCheckedAt: &checkedAt}, model.ProviderKeyQuotaRequest{})
	if err != nil {
		t.Fatalf("QueryOfficialQuota returned error: %v", err)
	}
	if !result.Supported || result.Status != "success" || result.Source != model.QuotaSourceResponseHeader || result.Unit != "requests" || result.Balance == nil || *result.Balance != 14523 || result.TotalQuantity == nil || *result.TotalQuantity != 477 || !result.FetchedAt.Equal(checkedAt) {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestQueryBraveQuotaFallsBackToLocalRequests(t *testing.T) {
	result, err := QueryOfficialQuota(context.Background(), model.APIKey{ProviderName: model.ProviderBrave, Alias: "brave", UsageRequestsTotal: 42}, model.ProviderKeyQuotaRequest{})
	if err != nil {
		t.Fatalf("QueryOfficialQuota returned error: %v", err)
	}
	if result.Source != model.QuotaSourceLocalMeter || result.Unit != "requests_used" || result.Balance != nil || result.TotalQuantity == nil || *result.TotalQuantity != 42 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func writeQuotaJSON(t *testing.T, w http.ResponseWriter, payload interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
