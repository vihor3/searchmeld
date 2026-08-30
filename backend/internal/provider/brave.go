package provider

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/one-search/one-search/backend/internal/model"
)

type BraveProvider struct {
	*HTTPProvider
}

func NewBraveProvider(cfg Config) *BraveProvider {
	cfg.Name = model.ProviderBrave
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.search.brave.com/res/v1"
	}
	return &BraveProvider{HTTPProvider: NewHTTPProvider(cfg)}
}

func (p *BraveProvider) Search(ctx context.Context, req model.SearchRequest, key model.APIKey) (model.ProviderResponse, error) {
	limit := requestLimit(req.Limit, 10, 20)
	params := url.Values{}
	params.Set("q", req.Query)
	params.Set("count", strconv.Itoa(limit))
	if freshness := braveFreshness(req); freshness != "" {
		params.Set("freshness", freshness)
	}
	if country := optionString(req.Options, "country"); country != "" {
		params.Set("country", country)
	}
	if lang := optionString(req.Options, "search_lang", "searchLang", "hl", "language"); lang != "" {
		params.Set("search_lang", lang)
	}
	if uiLang := optionString(req.Options, "ui_lang", "uiLang", "locale"); uiLang != "" {
		params.Set("ui_lang", uiLang)
	}
	if safesearch := optionString(req.Options, "safesearch", "safe_search", "safeSearch"); safesearch != "" {
		params.Set("safesearch", safesearch)
	}
	if offset := braveOffset(req, limit); offset > 0 {
		params.Set("offset", strconv.Itoa(offset))
	}
	if req.IncludeRaw {
		params.Set("extra_snippets", "true")
	}
	request, err := p.newGETRequest(ctx, "/web/search", params)
	if err != nil {
		return model.ProviderResponse{}, err
	}
	request.Header.Set("X-Subscription-Token", key.Value)
	response, err := p.client.Do(request)
	if err != nil {
		return model.ProviderResponse{}, err
	}
	responseHeader := response.Header.Clone()
	payload, err := p.decodeResponse(response)
	if err != nil {
		return model.ProviderResponse{}, err
	}
	results := normalizeBraveResults(payload, req.IncludeRaw)
	quota, _ := BraveQuotaFromHeaders(responseHeader, key)
	return model.ProviderResponse{Results: results, Usage: usageMeasurements(model.ProviderBrave, payload), Raw: payload, Quota: quota}, nil
}

type braveRateLimitWindow struct {
	Limit     float64
	Remaining float64
	Reset     float64
	Duration  int
}

// BraveQuotaFromHeaders extracts the longest rate-limit window from a normal
// Brave Search response. It never issues an additional upstream request.
func BraveQuotaFromHeaders(header http.Header, key model.APIKey) (*model.ProviderKeyQuotaResult, bool) {
	windows := parseBraveRateLimitWindows(header)
	if len(windows) == 0 {
		return nil, false
	}
	window := braveQuotaWindowByLargestDuration(windows)
	used := window.Limit - window.Remaining
	if used < 0 {
		used = 0
	}
	providerName := key.ProviderName
	if providerName == "" {
		providerName = model.ProviderBrave
	}
	return &model.ProviderKeyQuotaResult{
		Provider:      providerName,
		Alias:         key.Alias,
		Supported:     true,
		Status:        "success",
		Source:        model.QuotaSourceResponseHeader,
		Confidence:    model.QuotaConfidenceBestEffort,
		Unit:          "requests",
		Balance:       float64Pointer(window.Remaining),
		TotalQuantity: float64Pointer(used),
		Message:       "从正常 Brave 搜索响应的 X-RateLimit-* headers 更新，不额外消耗请求",
		Breakdown:     braveRateLimitBreakdown(windows),
		RawText:       braveRateLimitRawText(header),
		FetchedAt:     time.Now(),
	}, true
}

func parseBraveRateLimitWindows(header http.Header) []braveRateLimitWindow {
	limits := splitBraveHeaderNumbers(header.Get("X-RateLimit-Limit"))
	remaining := splitBraveHeaderNumbers(header.Get("X-RateLimit-Remaining"))
	resets := splitBraveHeaderNumbers(header.Get("X-RateLimit-Reset"))
	durations := parseBraveRateLimitDurations(header.Get("X-RateLimit-Policy"))
	count := maxBraveInt(len(limits), len(remaining), len(resets), len(durations))
	if count == 0 {
		return nil
	}
	windows := make([]braveRateLimitWindow, 0, count)
	for i := 0; i < count; i++ {
		windows = append(windows, braveRateLimitWindow{
			Limit:     braveValueAt(limits, i),
			Remaining: braveValueAt(remaining, i),
			Reset:     braveValueAt(resets, i),
			Duration:  int(braveValueAt(durations, i)),
		})
	}
	return windows
}

func splitBraveHeaderNumbers(value string) []float64 {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	numbers := make([]float64, 0, len(parts))
	for _, part := range parts {
		number, _ := strconv.ParseFloat(strings.TrimSpace(part), 64)
		numbers = append(numbers, number)
	}
	return numbers
}

func parseBraveRateLimitDurations(policy string) []float64 {
	if strings.TrimSpace(policy) == "" {
		return nil
	}
	parts := strings.Split(policy, ",")
	durations := make([]float64, 0, len(parts))
	for _, part := range parts {
		duration := 0.0
		sections := strings.Split(strings.TrimSpace(part), ";")
		for _, section := range sections[1:] {
			section = strings.TrimSpace(section)
			if strings.HasPrefix(section, "w=") {
				duration, _ = strconv.ParseFloat(strings.TrimPrefix(section, "w="), 64)
				break
			}
		}
		durations = append(durations, duration)
	}
	return durations
}

func braveQuotaWindowByLargestDuration(windows []braveRateLimitWindow) braveRateLimitWindow {
	sorted := append([]braveRateLimitWindow(nil), windows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Duration == sorted[j].Duration {
			return sorted[i].Limit > sorted[j].Limit
		}
		return sorted[i].Duration > sorted[j].Duration
	})
	return sorted[0]
}

func braveRateLimitBreakdown(windows []braveRateLimitWindow) []map[string]interface{} {
	breakdown := make([]map[string]interface{}, 0, len(windows))
	for _, window := range windows {
		breakdown = append(breakdown, map[string]interface{}{
			"limit":     window.Limit,
			"remaining": window.Remaining,
			"reset":     window.Reset,
			"window":    window.Duration,
		})
	}
	return breakdown
}

func braveRateLimitRawText(header http.Header) string {
	keys := []string{"X-RateLimit-Limit", "X-RateLimit-Policy", "X-RateLimit-Remaining", "X-RateLimit-Reset"}
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		if value := header.Get(key); value != "" {
			lines = append(lines, key+": "+value)
		}
	}
	return strings.Join(lines, "\n")
}

func braveValueAt(values []float64, index int) float64 {
	if index < 0 || index >= len(values) {
		return 0
	}
	return values[index]
}

func maxBraveInt(values ...int) int {
	max := 0
	for _, value := range values {
		if value > max {
			max = value
		}
	}
	return max
}

func braveFreshness(req model.SearchRequest) string {
	if value := optionString(req.Options, "freshness"); value != "" {
		return value
	}
	switch strings.ToLower(strings.TrimSpace(req.Freshness)) {
	case "day", "d", "qdr:d", "pd":
		return "pd"
	case "week", "w", "qdr:w", "pw":
		return "pw"
	case "month", "m", "qdr:m", "pm":
		return "pm"
	case "year", "y", "qdr:y", "py":
		return "py"
	default:
		return strings.TrimSpace(req.Freshness)
	}
}

func braveOffset(req model.SearchRequest, limit int) int {
	if offset := optionInt(req.Options, "offset"); offset > 0 {
		return offset
	}
	if page := optionInt(req.Options, "page"); page > 1 {
		return page - 1
	}
	if limit <= 0 {
		return 0
	}
	return 0
}

func normalizeBraveResults(payload map[string]interface{}, includeRaw bool) []model.SearchResult {
	web := mapFromInterface(payload["web"])
	if web == nil {
		return nil
	}
	items := resultArray(web, "results")
	results := make([]model.SearchResult, 0, len(items))
	for index, rawItem := range items {
		item := mapFromInterface(rawItem)
		if item == nil {
			continue
		}
		url := stringValue(item, "url")
		if url == "" {
			continue
		}
		snippet := stringValue(item, "description", "snippet")
		extraSnippets := stringArrayValue(item, "extra_snippets")
		content := snippet
		if len(extraSnippets) > 0 {
			content = strings.Join(append([]string{snippet}, extraSnippets...), "\n")
		}
		result := model.SearchResult{
			Title:       stringValue(item, "title"),
			URL:         url,
			Snippet:     truncate(snippet, 1000),
			Content:     truncate(content, 4000),
			Provider:    model.ProviderBrave,
			Providers:   []string{model.ProviderBrave},
			Score:       1 / float64(index+1),
			PublishedAt: parseTimeValue(stringValue(item, "age", "page_age", "published", "published_at", "date")),
		}
		if includeRaw {
			result.Raw = item
		}
		results = append(results, result)
	}
	return results
}
