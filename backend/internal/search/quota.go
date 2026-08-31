package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vihor3/searchmeld/backend/internal/model"
	"github.com/vihor3/searchmeld/backend/internal/provider"
)

const (
	defaultExaUsageBaseURL         = "https://admin-api.exa.ai/team-management/api-keys"
	defaultYouQuotaURL             = "https://api.you.com/v1/billing/account_balance"
	defaultJinaQuotaURL            = "https://r.jina.ai/"
	defaultTavilyUsageURL          = "https://api.tavily.com/usage"
	defaultFirecrawlCreditUsageURL = "https://api.firecrawl.dev/v2/team/credit-usage"
	defaultQuotaRequestTimeout     = 20 * time.Second
)

type quotaQueryConfig struct {
	exaUsageBaseURL         string
	youQuotaURL             string
	jinaQuotaURL            string
	tavilyUsageURL          string
	firecrawlCreditUsageURL string
	requestTimeout          time.Duration
}

func defaultQuotaQueryConfig() quotaQueryConfig {
	return quotaQueryConfig{
		exaUsageBaseURL:         defaultExaUsageBaseURL,
		youQuotaURL:             defaultYouQuotaURL,
		jinaQuotaURL:            defaultJinaQuotaURL,
		tavilyUsageURL:          defaultTavilyUsageURL,
		firecrawlCreditUsageURL: defaultFirecrawlCreditUsageURL,
		requestTimeout:          defaultQuotaRequestTimeout,
	}
}

// QueryOfficialQuota returns the best available quota or usage snapshot. The
// source field distinguishes official data from response metadata and local meters.
func QueryOfficialQuota(ctx context.Context, key model.APIKey, req model.ProviderKeyQuotaRequest) (model.ProviderKeyQuotaResult, error) {
	return queryOfficialQuota(ctx, key, req, defaultQuotaQueryConfig())
}

func queryOfficialQuota(ctx context.Context, key model.APIKey, req model.ProviderKeyQuotaRequest, config quotaQueryConfig) (model.ProviderKeyQuotaResult, error) {
	ctx, cancel := context.WithTimeout(ctx, config.requestTimeout)
	defer cancel()
	switch key.ProviderName {
	case model.ProviderExa:
		return queryExaQuota(ctx, key, req, config.exaUsageBaseURL)
	case model.ProviderYou:
		return queryYouQuota(ctx, key, config.youQuotaURL, req.ProxyURL)
	case model.ProviderJina:
		return queryJinaQuota(ctx, key, config.jinaQuotaURL, req.ProxyURL)
	case model.ProviderTavily:
		return queryTavilyQuota(ctx, key, config.tavilyUsageURL, req.ProxyURL)
	case model.ProviderFirecrawl:
		return queryFirecrawlQuota(ctx, key, config.firecrawlCreditUsageURL, req.ProxyURL)
	case model.ProviderSerper:
		return querySerperQuota(ctx, key)
	case model.ProviderBrave:
		return queryBraveQuota(ctx, key)
	default:
		return model.ProviderKeyQuotaResult{Provider: key.ProviderName, Alias: key.Alias, Supported: false, Status: "unsupported", Message: "该渠道暂未配置官方额度查询", FetchedAt: time.Now()}, nil
	}
}

func queryExaQuota(ctx context.Context, key model.APIKey, req model.ProviderKeyQuotaRequest, baseURL string) (model.ProviderKeyQuotaResult, error) {
	apiKeyID := strings.TrimSpace(req.ExaAPIKeyID)
	if apiKeyID == "" {
		apiKeyID = strings.TrimSpace(key.ExaAPIKeyID)
	}
	if apiKeyID == "" {
		apiKeyID = strings.TrimSpace(key.Value)
	}
	serviceKey := strings.TrimSpace(req.ExaServiceKey)
	if serviceKey == "" {
		serviceKey = strings.TrimSpace(key.ExaServiceKey)
	}
	if serviceKey == "" {
		return localExaQuota(key), nil
	}
	if apiKeyID == "" {
		return model.ProviderKeyQuotaResult{}, fmt.Errorf("Exa 官方 usage 查询需要 API Key ID")
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/" + url.PathEscape(apiKeyID) + "/usage"
	params := url.Values{}
	if strings.TrimSpace(req.StartDate) != "" {
		params.Set("start_date", strings.TrimSpace(req.StartDate))
	}
	if strings.TrimSpace(req.EndDate) != "" {
		params.Set("end_date", strings.TrimSpace(req.EndDate))
	}
	if strings.TrimSpace(req.GroupBy) != "" {
		params.Set("group_by", strings.TrimSpace(req.GroupBy))
	}
	if encoded := params.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return model.ProviderKeyQuotaResult{}, err
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("x-api-key", serviceKey)
	payload, err := doJSONQuotaRequest(httpReq, req.ProxyURL)
	if err != nil {
		return model.ProviderKeyQuotaResult{}, err
	}
	period := map[string]string{}
	if periodMap, ok := payload["period"].(map[string]interface{}); ok {
		period["start"] = stringFromAny(periodMap["start"])
		period["end"] = stringFromAny(periodMap["end"])
	}
	breakdown := objectArrayFromAny(payload["cost_breakdown"])
	totalCost := floatFromAny(payload["total_cost_usd"])
	totalQuantity := 0.0
	for _, item := range breakdown {
		totalQuantity += floatFromAny(item["quantity"])
	}
	return model.ProviderKeyQuotaResult{
		Provider:      key.ProviderName,
		Alias:         key.Alias,
		Supported:     true,
		Status:        "success",
		Source:        model.QuotaSourceOfficialUsage,
		Confidence:    model.QuotaConfidenceExact,
		Unit:          "usd_used",
		Message:       "Exa 官方接口返回指定周期用量/费用，非账户剩余额度",
		TotalCostUSD:  floatPtr(totalCost),
		TotalQuantity: floatPtr(totalQuantity),
		APIKeyID:      stringFromAny(payload["api_key_id"]),
		APIKeyName:    stringFromAny(payload["api_key_name"]),
		Period:        period,
		Breakdown:     breakdown,
		Raw:           payload,
		FetchedAt:     time.Now(),
	}, nil
}

func queryYouQuota(ctx context.Context, key model.APIKey, endpoint, proxyURL string) (model.ProviderKeyQuotaResult, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return model.ProviderKeyQuotaResult{}, err
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-API-Key", key.Value)
	payload, err := doJSONQuotaRequest(httpReq, proxyURL)
	if err != nil {
		return model.ProviderKeyQuotaResult{}, err
	}
	data, _ := payload["data"].(map[string]interface{})
	attributes, _ := data["attributes"].(map[string]interface{})
	balanceCents := floatFromAny(attributes["balance"])
	balanceUSD := balanceCents / 100
	return model.ProviderKeyQuotaResult{
		Provider:     key.ProviderName,
		Alias:        key.Alias,
		Supported:    true,
		Status:       "success",
		Source:       model.QuotaSourceOfficial,
		Confidence:   model.QuotaConfidenceExact,
		Unit:         "cents",
		Balance:      floatPtr(balanceCents),
		BalanceCents: floatPtr(balanceCents),
		BalanceUSD:   floatPtr(balanceUSD),
		AccountID:    stringFromAny(data["id"]),
		Raw:          payload,
		FetchedAt:    time.Now(),
	}, nil
}

func queryJinaQuota(ctx context.Context, key model.APIKey, endpoint, proxyURL string) (model.ProviderKeyQuotaResult, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return model.ProviderKeyQuotaResult{}, err
	}
	httpReq.Header.Set("Accept", "text/plain")
	httpReq.Header.Set("Authorization", "Bearer "+key.Value)
	client := quotaHTTPClient(ctx, proxyURL)
	resp, err := client.Do(httpReq)
	if err != nil {
		return model.ProviderKeyQuotaResult{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return model.ProviderKeyQuotaResult{}, err
	}
	text := string(body)
	if resp.StatusCode >= 400 {
		return model.ProviderKeyQuotaResult{}, fmt.Errorf("Jina quota query failed: status %d: %s", resp.StatusCode, truncateMessage(text, 500))
	}
	balanceMatch := regexp.MustCompile(`(?m)^\[Balance left\]\s+([+-]?[0-9]+(?:\.[0-9]+)?)\s*$`).FindStringSubmatch(text)
	if len(balanceMatch) < 2 {
		return localJinaQuota(key, text), nil
	}
	balance, _ := strconv.ParseFloat(balanceMatch[1], 64)
	accountID := ""
	accountMatch := regexp.MustCompile(`(?m)^\[Authenticated as\]\s+(.+?)\s*$`).FindStringSubmatch(text)
	if len(accountMatch) >= 2 {
		accountID = strings.TrimSpace(accountMatch[1])
	}
	return model.ProviderKeyQuotaResult{
		Provider:   key.ProviderName,
		Alias:      key.Alias,
		Supported:  true,
		Status:     "success",
		Source:     model.QuotaSourceProviderResponse,
		Confidence: model.QuotaConfidenceBestEffort,
		Unit:       "tokens",
		Balance:    floatPtr(balance),
		AccountID:  accountID,
		RawText:    text,
		FetchedAt:  time.Now(),
	}, nil
}

func queryTavilyQuota(ctx context.Context, key model.APIKey, endpoint, proxyURL string) (model.ProviderKeyQuotaResult, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return model.ProviderKeyQuotaResult{}, err
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+key.Value)
	payload, err := doJSONQuotaRequest(httpReq, proxyURL)
	if err != nil {
		return model.ProviderKeyQuotaResult{}, err
	}
	keyUsage, _ := payload["key"].(map[string]interface{})
	accountUsage, _ := payload["account"].(map[string]interface{})
	used := firstPositiveFloat(keyUsage, "usage", "search_usage")
	limit := firstPositiveFloat(keyUsage, "limit")
	balance := limit - used
	if limit <= 0 {
		planLimit := firstPositiveFloat(accountUsage, "plan_limit")
		paygoLimit := firstPositiveFloat(accountUsage, "paygo_limit")
		planUsage := firstPositiveFloat(accountUsage, "plan_usage")
		paygoUsage := firstPositiveFloat(accountUsage, "paygo_usage")
		limit = planLimit + paygoLimit
		used = planUsage + paygoUsage
		balance = limit - used
	}
	return model.ProviderKeyQuotaResult{
		Provider:      key.ProviderName,
		Alias:         key.Alias,
		Supported:     true,
		Status:        "success",
		Source:        model.QuotaSourceOfficial,
		Confidence:    model.QuotaConfidenceExact,
		Unit:          "credits",
		Balance:       floatPtr(balance),
		TotalQuantity: floatPtr(used),
		APIKeyName:    stringFromAny(keyUsage["name"]),
		AccountID:     stringFromAny(accountUsage["current_plan"]),
		Message:       "Tavily 官方 /usage 返回当前 API Key 用量和限额",
		Breakdown:     quotaBreakdown("key", keyUsage, "account", accountUsage),
		Raw:           payload,
		FetchedAt:     time.Now(),
	}, nil
}

func queryFirecrawlQuota(ctx context.Context, key model.APIKey, endpoint, proxyURL string) (model.ProviderKeyQuotaResult, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return model.ProviderKeyQuotaResult{}, err
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+key.Value)
	payload, err := doJSONQuotaRequest(httpReq, proxyURL)
	if err != nil {
		return model.ProviderKeyQuotaResult{}, err
	}
	data, _ := payload["data"].(map[string]interface{})
	remaining := firstNumber(data, "remainingCredits", "remaining_credits")
	planCredits := firstNumber(data, "planCredits", "plan_credits")
	used := planCredits - remaining
	if used < 0 {
		used = 0
	}
	period := map[string]string{}
	if start := stringFromAny(data["billingPeriodStart"]); start != "" {
		period["start"] = start
	}
	if end := stringFromAny(data["billingPeriodEnd"]); end != "" {
		period["end"] = end
	}
	return model.ProviderKeyQuotaResult{
		Provider:      key.ProviderName,
		Alias:         key.Alias,
		Supported:     true,
		Status:        "success",
		Source:        model.QuotaSourceOfficial,
		Confidence:    model.QuotaConfidenceExact,
		Unit:          "credits",
		Balance:       floatPtr(remaining),
		TotalQuantity: floatPtr(used),
		Message:       "Firecrawl 官方 /v2/team/credit-usage 返回团队剩余 credits",
		Period:        period,
		Breakdown:     quotaBreakdown("data", data),
		Raw:           payload,
		FetchedAt:     time.Now(),
	}, nil
}

const serperDefaultCredits = 2500

func querySerperQuota(ctx context.Context, key model.APIKey) (model.ProviderKeyQuotaResult, error) {
	// 只用本地 credits meter，不用 requests 次数冒充 credits。
	used := key.UsageCreditsTotal
	if used < 0 {
		used = 0
	}
	balance := float64(serperDefaultCredits) - used
	return model.ProviderKeyQuotaResult{
		Provider:      key.ProviderName,
		Alias:         key.Alias,
		Supported:     true,
		Status:        "success",
		Source:        model.QuotaSourceLocalMeter,
		Confidence:    model.QuotaConfidenceEstimated,
		Unit:          "credits",
		Balance:       floatPtr(balance),
		TotalQuantity: floatPtr(used),
		Message:       "Serper 未公开独立余额接口；按注册赠送的 2500 credits 减本实例全生命周期累计 credits 估算剩余额度",
		Breakdown:     quotaBreakdown("default", map[string]interface{}{"credits": serperDefaultCredits}, "local", map[string]interface{}{"credits_total": key.UsageCreditsTotal, "requests_total": key.UsageRequestsTotal}),
		FetchedAt:     time.Now(),
	}, nil
}

func queryBraveQuota(ctx context.Context, key model.APIKey) (model.ProviderKeyQuotaResult, error) {
	_ = ctx
	if key.OfficialQuotaSource == model.QuotaSourceResponseHeader && key.OfficialQuotaCheckedAt != nil && key.OfficialQuotaBalance != nil {
		message := "来自最近一次正常 Brave 搜索响应头，不会为查询额度额外消耗请求"
		if strings.TrimSpace(key.OfficialQuotaMessage) != "" {
			message = key.OfficialQuotaMessage
		}
		return model.ProviderKeyQuotaResult{
			Provider:      key.ProviderName,
			Alias:         key.Alias,
			Supported:     true,
			Status:        "success",
			Source:        model.QuotaSourceResponseHeader,
			Confidence:    firstQuotaString(key.OfficialQuotaConfidence, model.QuotaConfidenceBestEffort),
			Unit:          firstQuotaString(key.OfficialQuotaUnit, "requests"),
			Balance:       key.OfficialQuotaBalance,
			TotalQuantity: key.OfficialQuotaTotalQuantity,
			Message:       message,
			FetchedAt:     *key.OfficialQuotaCheckedAt,
		}, nil
	}
	used := float64(key.UsageRequestsTotal)
	return model.ProviderKeyQuotaResult{
		Provider:      key.ProviderName,
		Alias:         key.Alias,
		Supported:     true,
		Status:        "success",
		Source:        model.QuotaSourceLocalMeter,
		Confidence:    model.QuotaConfidenceExact,
		Unit:          "requests_used",
		TotalQuantity: floatPtr(used),
		Message:       "尚未从正常 Brave 搜索响应中获得额度头；当前仅显示本实例全生命周期请求数",
		Breakdown:     quotaBreakdown("local", map[string]interface{}{"requests_total": key.UsageRequestsTotal}),
		FetchedAt:     time.Now(),
	}, nil
}

func localExaQuota(key model.APIKey) model.ProviderKeyQuotaResult {
	used := key.UsageCostUSDTotal
	requests := float64(key.UsageRequestsTotal)
	return model.ProviderKeyQuotaResult{
		Provider:      key.ProviderName,
		Alias:         key.Alias,
		Supported:     true,
		Status:        "success",
		Source:        model.QuotaSourceLocalMeter,
		Confidence:    model.QuotaConfidenceEstimated,
		Unit:          "usd_used",
		TotalCostUSD:  floatPtr(used),
		TotalQuantity: floatPtr(requests),
		Message:       "未配置 Exa Team Management 密钥；显示本实例根据上游 costDollars 或公开单价累计的费用",
		Breakdown:     quotaBreakdown("local", map[string]interface{}{"requests_total": key.UsageRequestsTotal, "cost_usd_total": used}),
		FetchedAt:     time.Now(),
	}
}

func localJinaQuota(key model.APIKey, rawText string) model.ProviderKeyQuotaResult {
	used := key.UsageTokensTotal
	return model.ProviderKeyQuotaResult{
		Provider:      key.ProviderName,
		Alias:         key.Alias,
		Supported:     true,
		Status:        "success",
		Source:        model.QuotaSourceLocalMeter,
		Confidence:    model.QuotaConfidenceBestEffort,
		Unit:          "tokens_used",
		TotalQuantity: floatPtr(used),
		Message:       "Jina 根地址未返回可解析的 Balance left；显示本实例从上游响应累计的 tokens",
		Breakdown:     quotaBreakdown("local", map[string]interface{}{"tokens_total": used, "requests_total": key.UsageRequestsTotal}),
		RawText:       rawText,
		FetchedAt:     time.Now(),
	}
}

func doJSONQuotaRequest(req *http.Request, proxyURL string) (map[string]interface{}, error) {
	payload, _, err := doJSONQuotaRequestWithHeader(req, proxyURL)
	return payload, err
}

func doJSONQuotaRequestWithHeader(req *http.Request, proxyURL string) (map[string]interface{}, http.Header, error) {
	client := quotaHTTPClient(req.Context(), proxyURL)
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, resp.Header, err
	}
	if resp.StatusCode >= 400 {
		return nil, resp.Header, fmt.Errorf("official quota query failed: status %d: %s", resp.StatusCode, truncateMessage(string(body), 500))
	}
	var payload map[string]interface{}
	if len(strings.TrimSpace(string(body))) == 0 {
		payload = map[string]interface{}{}
	} else if err := json.Unmarshal(body, &payload); err != nil {
		return nil, resp.Header, fmt.Errorf("decode official quota response: %w", err)
	}
	return payload, resp.Header, nil
}

func quotaHTTPClient(ctx context.Context, proxyURL string) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if normalized := provider.NormalizeProxyURL(proxyURL); normalized != "" {
		if parsed, err := url.Parse(normalized); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	timeout := defaultQuotaRequestTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
		if timeout <= 0 {
			timeout = time.Nanosecond
		}
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

func firstPositiveFloat(values map[string]interface{}, keys ...string) float64 {
	for _, key := range keys {
		value := floatFromAny(values[key])
		if value > 0 {
			return value
		}
	}
	return 0
}

func firstNumber(values map[string]interface{}, keys ...string) float64 {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return floatFromAny(value)
		}
	}
	return 0
}

func firstQuotaString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func quotaBreakdown(parts ...interface{}) []map[string]interface{} {
	breakdown := []map[string]interface{}{}
	for i := 0; i+1 < len(parts); i += 2 {
		name := fmt.Sprint(parts[i])
		values, ok := parts[i+1].(map[string]interface{})
		if !ok || len(values) == 0 {
			continue
		}
		item := map[string]interface{}{"scope": name}
		for key, value := range values {
			item[key] = value
		}
		breakdown = append(breakdown, item)
	}
	return breakdown
}

func objectArrayFromAny(value interface{}) []map[string]interface{} {
	items, ok := value.([]interface{})
	if !ok {
		return nil
	}
	result := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		if mapped, ok := item.(map[string]interface{}); ok {
			result = append(result, mapped)
		}
	}
	return result
}

func stringFromAny(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return typed
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

func floatFromAny(value interface{}) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case json.Number:
		parsed, _ := typed.Float64()
		return parsed
	case string:
		parsed, _ := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed
	default:
		return 0
	}
}

func floatPtr(value float64) *float64 {
	return &value
}

func truncateMessage(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
