package provider

import (
	"context"
	"net/http"
	"strings"

	"github.com/one-search/one-search/backend/internal/model"
)

type ExaProvider struct {
	*HTTPProvider
}

func NewExaProvider(cfg Config) *ExaProvider {
	cfg.Name = model.ProviderExa
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.exa.ai"
	}
	return &ExaProvider{HTTPProvider: NewHTTPProvider(cfg)}
}

func (p *ExaProvider) Search(ctx context.Context, req model.SearchRequest, key model.APIKey) (model.ProviderResponse, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	body := map[string]interface{}{
		"query":      req.Query,
		"numResults": limit,
		"type":       "neural",
		"contents": map[string]interface{}{
			"text":       true,
			"highlights": true,
		},
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
	results := normalizeExaResults(payload, req.IncludeRaw)
	return model.ProviderResponse{Results: results, Usage: usageMeasurements(model.ProviderExa, payload), Raw: payload}, nil
}

func (p *ExaProvider) Extract(ctx context.Context, req model.ExtractRequest, key model.APIKey) (model.ExtractProviderResponse, error) {
	body := map[string]interface{}{
		"urls": req.URLs,
		"text": true,
	}
	if strings.TrimSpace(req.Query) != "" {
		body["highlights"] = map[string]interface{}{"query": req.Query}
	}
	if maxCharacters := optionInt(req.Options, "max_characters", "maxCharacters"); maxCharacters > 0 {
		body["text"] = map[string]interface{}{"maxCharacters": maxCharacters}
	}

	request, err := p.newJSONRequest(ctx, http.MethodPost, "/contents", body)
	if err != nil {
		return model.ExtractProviderResponse{}, err
	}
	request.Header.Set("x-api-key", key.Value)
	response, err := p.client.Do(request)
	if err != nil {
		return model.ExtractProviderResponse{}, err
	}
	payload, err := p.decodeResponse(response)
	if err != nil {
		return model.ExtractProviderResponse{}, err
	}
	results, failures := normalizeExaExtractResults(payload, req)
	return model.ExtractProviderResponse{
		Results:       results,
		FailedResults: failures,
		Usage:         usageMeasurements(model.ProviderExa, payload),
		Raw:           payload,
	}, nil
}

func normalizeExaExtractResults(payload map[string]interface{}, req model.ExtractRequest) ([]model.ExtractResult, []model.ExtractFailure) {
	items := resultArray(payload, "results")
	results := make([]model.ExtractResult, 0, len(items))
	for _, rawItem := range items {
		item := mapFromInterface(rawItem)
		if item == nil {
			continue
		}
		// For URL requests Exa keeps the caller's value in id while url may be a
		// redirect/canonical target. Preserve id for fallback correlation.
		resultURL := stringValue(item, "id", "url")
		if resultURL == "" {
			continue
		}
		content := stringValue(item, "text", "content")
		if strings.TrimSpace(req.Query) != "" {
			if highlights := stringArrayValue(item, "highlights"); len(highlights) > 0 {
				content = strings.Join(highlights, "\n\n")
			}
		}
		if req.Format == model.ExtractFormatText {
			// Exa's contents API returns Markdown for both text and highlights.
			// Keep the response contract honest when callers request plain text.
			content = markdownToPlainText(content)
		}
		result := model.ExtractResult{
			URL:         resultURL,
			Title:       stringValue(item, "title"),
			Content:     content,
			Format:      requestedExtractFormat(req.Format),
			Provider:    model.ProviderExa,
			Providers:   []string{model.ProviderExa},
			Images:      stringArrayValue(item, "images"),
			Favicon:     stringValue(item, "favicon"),
			Author:      stringValue(item, "author"),
			PublishedAt: parseTimeValue(stringValue(item, "publishedDate", "published_at")),
		}
		if imageURL := stringValue(item, "image"); imageURL != "" {
			result.Images = appendUniqueString(result.Images, imageURL)
		}
		if req.IncludeRaw {
			result.Raw = item
		}
		results = append(results, result)
	}

	failures := make([]model.ExtractFailure, 0)
	for _, rawStatus := range resultArray(payload, "statuses") {
		status := mapFromInterface(rawStatus)
		if status == nil {
			continue
		}
		state := strings.ToLower(strings.TrimSpace(stringValue(status, "status")))
		if state == "" || state == "success" || state == "completed" {
			continue
		}
		errorMessage := stringValue(status, "error", "message")
		if errorDetails := mapFromInterface(status["error"]); errorDetails != nil {
			errorMessage = firstNonEmpty(stringValue(errorDetails, "message", "tag", "error"), errorMessage)
		}
		failures = append(failures, model.ExtractFailure{
			URL:       stringValue(status, "id", "url"),
			Provider:  model.ProviderExa,
			ErrorType: ErrorTypeUpstream,
			Error:     firstNonEmpty(errorMessage, "Exa could not extract this URL"),
		})
	}
	return results, failures
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func requestedExtractFormat(format model.ExtractFormat) model.ExtractFormat {
	switch format {
	case model.ExtractFormatText:
		return model.ExtractFormatText
	default:
		// Exa's contents endpoint returns its extracted text representation; it
		// does not provide source HTML through this call.
		return model.ExtractFormatMarkdown
	}
}

func normalizeExaResults(payload map[string]interface{}, includeRaw bool) []model.SearchResult {
	items := resultArray(payload, "results")
	results := make([]model.SearchResult, 0, len(items))
	for _, rawItem := range items {
		item := mapFromInterface(rawItem)
		if item == nil {
			continue
		}
		url := stringValue(item, "url")
		if url == "" {
			continue
		}
		snippet := firstStringFromArray(item, "highlights")
		if snippet == "" {
			snippet = stringValue(item, "text", "summary")
		}
		result := model.SearchResult{
			Title:       stringValue(item, "title"),
			URL:         url,
			Snippet:     snippet,
			Content:     stringValue(item, "text"),
			Provider:    model.ProviderExa,
			Providers:   []string{model.ProviderExa},
			Score:       floatValue(item, "score"),
			PublishedAt: parseTimeValue(stringValue(item, "publishedDate", "published_at")),
		}
		if includeRaw {
			result.Raw = item
		}
		results = append(results, result)
	}
	return results
}
