package provider

import (
	"context"
	"net/http"
	"strings"

	"github.com/one-search/one-search/backend/internal/model"
)

type TavilyProvider struct {
	*HTTPProvider
}

func NewTavilyProvider(cfg Config) *TavilyProvider {
	cfg.Name = model.ProviderTavily
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.tavily.com"
	}
	return &TavilyProvider{HTTPProvider: NewHTTPProvider(cfg)}
}

func (p *TavilyProvider) Search(ctx context.Context, req model.SearchRequest, key model.APIKey) (model.ProviderResponse, error) {
	body := map[string]interface{}{
		"query":         req.Query,
		"max_results":   requestLimit(req.Limit, 10, 20),
		"include_usage": true,
	}
	if searchDepth := optionString(req.Options, "search_depth"); searchDepth != "" {
		body["search_depth"] = searchDepth
	}
	if topic := optionString(req.Options, "topic"); topic != "" {
		body["topic"] = topic
	}
	if timeRange := tavilyTimeRange(req); timeRange != "" {
		body["time_range"] = timeRange
	}
	if country := optionString(req.Options, "country"); country != "" {
		body["country"] = country
	}
	if includeDomains := optionStringSlice(req.Options, "include_domains", "includeDomains"); len(includeDomains) > 0 {
		body["include_domains"] = includeDomains
	}
	if excludeDomains := optionStringSlice(req.Options, "exclude_domains", "excludeDomains"); len(excludeDomains) > 0 {
		body["exclude_domains"] = excludeDomains
	}
	if req.IncludeRaw {
		body["include_raw_content"] = true
	}
	request, err := p.newJSONRequest(ctx, http.MethodPost, "/search", body)
	if err != nil {
		return model.ProviderResponse{}, err
	}
	request.Header.Set("Authorization", "Bearer "+key.Value)
	response, err := p.client.Do(request)
	if err != nil {
		return model.ProviderResponse{}, err
	}
	payload, err := p.decodeResponse(response)
	if err != nil {
		return model.ProviderResponse{}, err
	}
	results := normalizeTavilyResults(payload, req.IncludeRaw)
	return model.ProviderResponse{Results: results, Usage: usageMeasurements(model.ProviderTavily, payload), Raw: payload}, nil
}

func (p *TavilyProvider) Extract(ctx context.Context, req model.ExtractRequest, key model.APIKey) (model.ExtractProviderResponse, error) {
	body := map[string]interface{}{
		"urls":          req.URLs,
		"include_usage": true,
		"format":        tavilyExtractFormat(req.Format),
	}
	if query := strings.TrimSpace(req.Query); query != "" {
		body["query"] = query
	}
	if depth := strings.TrimSpace(req.ExtractDepth); depth != "" {
		body["extract_depth"] = depth
	}
	if req.ChunksPerSource > 0 {
		body["chunks_per_source"] = req.ChunksPerSource
	}
	if req.IncludeImages {
		body["include_images"] = true
	}
	if req.IncludeFavicon {
		body["include_favicon"] = true
	}
	if timeout := optionFloat(req.Options, "timeout"); timeout > 0 {
		body["timeout"] = timeout
	}

	request, err := p.newJSONRequest(ctx, http.MethodPost, "/extract", body)
	if err != nil {
		return model.ExtractProviderResponse{}, err
	}
	request.Header.Set("Authorization", "Bearer "+key.Value)
	response, err := p.client.Do(request)
	if err != nil {
		return model.ExtractProviderResponse{}, err
	}
	payload, err := p.decodeResponse(response)
	if err != nil {
		return model.ExtractProviderResponse{}, err
	}
	results, failures := normalizeTavilyExtractResults(payload, req)
	return model.ExtractProviderResponse{
		Results:       results,
		FailedResults: failures,
		Usage:         usageMeasurements(model.ProviderTavily, payload),
		Raw:           payload,
	}, nil
}

func tavilyExtractFormat(format model.ExtractFormat) string {
	switch format {
	case model.ExtractFormatText:
		return string(model.ExtractFormatText)
	default:
		return string(model.ExtractFormatMarkdown)
	}
}

func normalizeTavilyExtractResults(payload map[string]interface{}, req model.ExtractRequest) ([]model.ExtractResult, []model.ExtractFailure) {
	items := resultArray(payload, "results")
	results := make([]model.ExtractResult, 0, len(items))
	for _, rawItem := range items {
		item := mapFromInterface(rawItem)
		if item == nil {
			continue
		}
		resultURL := stringValue(item, "url")
		if resultURL == "" {
			continue
		}
		result := model.ExtractResult{
			URL:       resultURL,
			Title:     stringValue(item, "title"),
			Content:   stringValue(item, "raw_content", "content"),
			Format:    model.ExtractFormat(tavilyExtractFormat(req.Format)),
			Provider:  model.ProviderTavily,
			Providers: []string{model.ProviderTavily},
			Images:    stringArrayValue(item, "images"),
			Favicon:   stringValue(item, "favicon"),
			Author:    stringValue(item, "author"),
			PublishedAt: parseTimeValue(stringValue(item,
				"published_date", "publishedDate", "date")),
		}
		if req.IncludeRaw {
			result.Raw = item
		}
		results = append(results, result)
	}

	failures := make([]model.ExtractFailure, 0)
	for _, rawItem := range resultArray(payload, "failed_results") {
		item := mapFromInterface(rawItem)
		if item == nil {
			continue
		}
		failures = append(failures, model.ExtractFailure{
			URL:       stringValue(item, "url"),
			Provider:  model.ProviderTavily,
			ErrorType: ErrorTypeUpstream,
			Error:     firstNonEmpty(stringValue(item, "error", "message"), "Tavily could not extract this URL"),
		})
	}
	return results, failures
}

func tavilyTimeRange(req model.SearchRequest) string {
	if value := optionString(req.Options, "time_range", "timeRange"); value != "" {
		return value
	}
	switch strings.ToLower(strings.TrimSpace(req.Freshness)) {
	case "day", "d", "qdr:d", "pd":
		return "day"
	case "week", "w", "qdr:w", "pw":
		return "week"
	case "month", "m", "qdr:m", "pm":
		return "month"
	case "year", "y", "qdr:y", "py":
		return "year"
	}
	switch days := optionInt(req.Options, "days"); {
	case days <= 0:
		return ""
	case days <= 1:
		return "day"
	case days <= 7:
		return "week"
	case days <= 31:
		return "month"
	default:
		return "year"
	}
}

func normalizeTavilyResults(payload map[string]interface{}, includeRaw bool) []model.SearchResult {
	items := resultArray(payload, "results")
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
		content := stringValue(item, "raw_content", "content")
		snippet := stringValue(item, "content", "snippet", "description")
		if snippet == "" {
			snippet = content
		}
		score := floatValue(item, "score")
		if score == 0 {
			score = 1 / float64(index+1)
		}
		result := model.SearchResult{
			Title:       stringValue(item, "title"),
			URL:         url,
			Snippet:     truncate(snippet, 1000),
			Content:     truncate(content, 4000),
			Provider:    model.ProviderTavily,
			Providers:   []string{model.ProviderTavily},
			Score:       score,
			PublishedAt: parseTimeValue(stringValue(item, "published_date", "publishedDate", "date")),
		}
		if includeRaw {
			result.Raw = item
		}
		results = append(results, result)
	}
	return results
}
