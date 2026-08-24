package provider

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/one-search/one-search/backend/internal/model"
)

type JinaProvider struct {
	*HTTPProvider
	reader *HTTPProvider
}

func (p *JinaProvider) ExtractBatchSize() int {
	return 1
}

func NewJinaProvider(cfg Config) *JinaProvider {
	readerCfg := cfg
	cfg.Name = model.ProviderJina
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://s.jina.ai"
	}
	readerCfg.Name = model.ProviderJina
	readerCfg.BaseURL = strings.TrimSpace(cfg.ExtractBaseURL)
	if readerCfg.BaseURL == "" {
		readerCfg.BaseURL = jinaReaderBaseURL(cfg.BaseURL)
	}
	return &JinaProvider{HTTPProvider: NewHTTPProvider(cfg), reader: NewHTTPProvider(readerCfg)}
}

func jinaReaderBaseURL(searchBaseURL string) string {
	value := strings.TrimRight(strings.TrimSpace(searchBaseURL), "/")
	if value == "" || value == "https://s.jina.ai" || value == "http://s.jina.ai" {
		return strings.Replace(firstNonEmpty(value, "https://s.jina.ai"), "://s.jina.ai", "://r.jina.ai", 1)
	}
	// A custom base URL is commonly an API-compatible proxy or an httptest
	// server, so use it for both search and reader requests.
	return value
}

func (p *JinaProvider) Search(ctx context.Context, req model.SearchRequest, key model.APIKey) (model.ProviderResponse, error) {
	request, err := p.newGETRequest(ctx, "/"+url.PathEscape(req.Query), nil)
	if err != nil {
		return model.ProviderResponse{}, err
	}
	request.Header.Set("Accept", "application/json")
	if key.Value != "" {
		request.Header.Set("Authorization", "Bearer "+key.Value)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return model.ProviderResponse{}, err
	}
	payload, err := p.decodeResponse(response)
	if err != nil {
		return model.ProviderResponse{}, err
	}
	results := normalizeJinaResults(payload, req.IncludeRaw)
	return model.ProviderResponse{Results: results, Usage: usageMeasurements(model.ProviderJina, payload), Raw: payload}, nil
}

func (p *JinaProvider) Extract(ctx context.Context, req model.ExtractRequest, key model.APIKey) (model.ExtractProviderResponse, error) {
	result := model.ExtractProviderResponse{
		Results:       make([]model.ExtractResult, 0, len(req.URLs)),
		FailedResults: make([]model.ExtractFailure, 0),
		Raw:           map[string]interface{}{"responses": []interface{}{}},
	}
	rawResponses := make([]interface{}, 0, len(req.URLs))
	for _, targetURL := range req.URLs {
		request, err := p.jinaReaderRequest(ctx, targetURL, req.Format)
		if err != nil {
			return model.ExtractProviderResponse{}, err
		}
		if key.Value != "" {
			request.Header.Set("Authorization", "Bearer "+key.Value)
		}
		response, err := p.reader.client.Do(request)
		if err != nil {
			return model.ExtractProviderResponse{}, err
		}
		payload, err := p.reader.decodeResponse(response)
		if err != nil {
			if shouldRetryExtractRequest(err) {
				return model.ExtractProviderResponse{}, err
			}
			result.FailedResults = append(result.FailedResults, model.ExtractFailure{
				URL: targetURL, Provider: model.ProviderJina, ErrorType: ErrorType(err), Error: err.Error(),
			})
			continue
		}
		rawResponses = append(rawResponses, payload)
		result.Usage = append(result.Usage, usageMeasurements(model.ProviderJina, payload)...)
		item, failure := normalizeJinaExtractResult(payload, targetURL, req)
		if failure != nil {
			result.FailedResults = append(result.FailedResults, *failure)
			continue
		}
		result.Results = append(result.Results, item)
	}
	result.Raw["responses"] = rawResponses
	return result, nil
}

func (p *JinaProvider) jinaReaderRequest(ctx context.Context, targetURL string, format model.ExtractFormat) (*http.Request, error) {
	request, err := p.reader.newGETRequest(ctx, "/"+targetURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	switch format {
	case model.ExtractFormatText:
		request.Header.Set("X-Return-Format", "text")
	case model.ExtractFormatHTML, model.ExtractFormatRawHTML:
		request.Header.Set("X-Return-Format", "html")
	default:
		request.Header.Set("X-Return-Format", "markdown")
	}
	return request, nil
}

func normalizeJinaExtractResult(payload map[string]interface{}, targetURL string, req model.ExtractRequest) (model.ExtractResult, *model.ExtractFailure) {
	item := mapFromInterface(payload["data"])
	if item == nil {
		item = payload
	}
	content := stringValue(item, "content", "text", "markdown", "html")
	if content == "" {
		return model.ExtractResult{}, &model.ExtractFailure{
			URL: targetURL, Provider: model.ProviderJina, ErrorType: ErrorTypeInvalidResponse, Error: "Jina Reader returned no content",
		}
	}
	result := model.ExtractResult{
		// Keep the caller's URL as the correlation key even when Reader reports
		// a redirect target or canonical URL.
		URL:         targetURL,
		Title:       stringValue(item, "title"),
		Content:     content,
		Format:      jinaExtractFormat(req.Format),
		Provider:    model.ProviderJina,
		Providers:   []string{model.ProviderJina},
		Images:      stringArrayValue(item, "images"),
		Favicon:     stringValue(item, "favicon"),
		Author:      stringValue(item, "author"),
		PublishedAt: parseTimeValue(stringValue(item, "publishedTime", "published_at", "date")),
	}
	if req.IncludeRaw {
		result.Raw = item
	}
	return result, nil
}

func jinaExtractFormat(format model.ExtractFormat) model.ExtractFormat {
	switch format {
	case model.ExtractFormatText:
		return model.ExtractFormatText
	case model.ExtractFormatHTML, model.ExtractFormatRawHTML:
		// Reader's HTML mode is rendered HTML, not the untouched source body.
		return model.ExtractFormatHTML
	default:
		return model.ExtractFormatMarkdown
	}
}

func normalizeJinaResults(payload map[string]interface{}, includeRaw bool) []model.SearchResult {
	items := resultArray(payload, "data", "results")
	results := make([]model.SearchResult, 0, len(items))
	for index, rawItem := range items {
		item := mapFromInterface(rawItem)
		if item == nil {
			continue
		}
		url := stringValue(item, "url", "link")
		if url == "" {
			continue
		}
		score := floatValue(item, "score")
		if score == 0 {
			score = 1 / float64(index+1)
		}
		result := model.SearchResult{
			Title:       stringValue(item, "title"),
			URL:         url,
			Snippet:     truncate(stringValue(item, "description", "snippet", "content"), 1000),
			Content:     truncate(stringValue(item, "content", "text", "description"), 4000),
			Provider:    model.ProviderJina,
			Providers:   []string{model.ProviderJina},
			Score:       score,
			PublishedAt: parseTimeValue(stringValue(item, "published", "published_at", "date")),
		}
		if includeRaw {
			result.Raw = item
		}
		results = append(results, result)
	}
	return results
}
