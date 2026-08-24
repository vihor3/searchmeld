package provider

import (
	"context"
	"encoding/json"
	"html"
	"math"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/one-search/one-search/backend/internal/model"
)

type FirecrawlProvider struct {
	*HTTPProvider
}

func (p *FirecrawlProvider) ExtractBatchSize() int {
	return 1
}

func NewFirecrawlProvider(cfg Config) *FirecrawlProvider {
	cfg.Name = model.ProviderFirecrawl
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.firecrawl.dev"
	}
	return &FirecrawlProvider{HTTPProvider: NewHTTPProvider(cfg)}
}

func (p *FirecrawlProvider) Search(ctx context.Context, req model.SearchRequest, key model.APIKey) (model.ProviderResponse, error) {
	body := map[string]interface{}{
		"query":   req.Query,
		"limit":   requestLimit(req.Limit, 10, 100),
		"sources": []string{"web"},
	}
	if tbs := firecrawlTBS(req); tbs != "" {
		body["tbs"] = tbs
	}
	if country := optionString(req.Options, "country"); country != "" {
		body["country"] = country
	}
	if location := optionString(req.Options, "location"); location != "" {
		body["location"] = location
	}
	if includeDomains := optionStringSlice(req.Options, "include_domains", "includeDomains"); len(includeDomains) > 0 {
		body["includeDomains"] = includeDomains
	}
	if excludeDomains := optionStringSlice(req.Options, "exclude_domains", "excludeDomains"); len(excludeDomains) > 0 {
		body["excludeDomains"] = excludeDomains
	}
	if timeout := optionInt(req.Options, "timeout"); timeout > 0 {
		body["timeout"] = timeout
	}
	if req.IncludeRaw {
		body["scrapeOptions"] = map[string]interface{}{"formats": []string{"markdown"}}
	}
	request, err := p.newJSONRequest(ctx, http.MethodPost, "/v2/search", body)
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
	results := normalizeFirecrawlResults(payload, req.IncludeRaw)
	return model.ProviderResponse{Results: results, Usage: usageMeasurements(model.ProviderFirecrawl, payload), Raw: payload}, nil
}

func (p *FirecrawlProvider) Extract(ctx context.Context, req model.ExtractRequest, key model.APIKey) (model.ExtractProviderResponse, error) {
	result := model.ExtractProviderResponse{
		Results:       make([]model.ExtractResult, 0, len(req.URLs)),
		FailedResults: make([]model.ExtractFailure, 0),
		Raw:           map[string]interface{}{"responses": []interface{}{}},
	}
	rawResponses := make([]interface{}, 0, len(req.URLs))
	for _, targetURL := range req.URLs {
		body := firecrawlExtractBody(targetURL, req)
		request, err := p.newJSONRequest(ctx, http.MethodPost, "/v2/scrape", body)
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
			if shouldRetryExtractRequest(err) {
				return model.ExtractProviderResponse{}, err
			}
			result.FailedResults = append(result.FailedResults, model.ExtractFailure{
				URL: targetURL, Provider: model.ProviderFirecrawl, ErrorType: ErrorType(err), Error: err.Error(),
			})
			continue
		}
		rawResponses = append(rawResponses, payload)
		result.Usage = append(result.Usage, usageMeasurements(model.ProviderFirecrawl, payload)...)
		item, failure := normalizeFirecrawlExtractResult(payload, targetURL, req)
		if failure != nil {
			result.FailedResults = append(result.FailedResults, *failure)
			continue
		}
		result.Results = append(result.Results, item)
	}
	result.Raw["responses"] = rawResponses
	return result, nil
}

func firecrawlExtractBody(targetURL string, req model.ExtractRequest) map[string]interface{} {
	format := "markdown"
	switch req.Format {
	case model.ExtractFormatHTML:
		format = "html"
	case model.ExtractFormatRawHTML:
		format = "rawHtml"
	}
	formats := []string{format}
	if req.IncludeImages {
		formats = append(formats, "images")
	}
	body := map[string]interface{}{
		"url":     targetURL,
		"formats": formats,
	}
	copyExtractOption(body, req.Options, "onlyMainContent", "only_main_content", "onlyMainContent")
	copyExtractOption(body, req.Options, "includeTags", "include_tags", "includeTags")
	copyExtractOption(body, req.Options, "excludeTags", "exclude_tags", "excludeTags")
	copyExtractOption(body, req.Options, "waitFor", "wait_for", "waitFor")
	if timeout, ok := firecrawlExtractTimeout(req); ok {
		body["timeout"] = timeout
	}
	copyExtractOption(body, req.Options, "mobile", "mobile")
	copyExtractOption(body, req.Options, "skipTlsVerification", "skip_tls_verification", "skipTlsVerification")
	copyExtractOption(body, req.Options, "removeBase64Images", "remove_base64_images", "removeBase64Images")
	copyExtractOption(body, req.Options, "blockAds", "block_ads", "blockAds")
	copyExtractOption(body, req.Options, "proxy", "proxy")
	return body
}

func firecrawlExtractTimeout(req model.ExtractRequest) (interface{}, bool) {
	value, ok := req.Options["timeout"]
	if !ok {
		return nil, false
	}
	if req.CompatFormat != model.CompatFormatTavily {
		// Native Firecrawl options use the upstream unit (milliseconds).
		return value, true
	}

	seconds, ok := firecrawlNumericValue(value)
	if !ok {
		// Compatibility validation normally rejects this before the adapter.
		// Preserve the value here so direct adapter callers still receive an
		// upstream/encoding error rather than a misleading unit conversion.
		return value, true
	}
	milliseconds := math.Ceil(seconds * 1000)
	// Keep the emitted value inside Firecrawl's accepted 1s-300s range even
	// when the adapter is called directly without compatibility validation.
	if milliseconds < 1000 {
		milliseconds = 1000
	}
	if milliseconds > 300000 {
		milliseconds = 300000
	}
	return int64(milliseconds), true
}

func firecrawlNumericValue(value interface{}) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float32:
		parsed := float64(typed)
		if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0, false
		}
		return parsed, true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, false
		}
		return typed, true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func copyExtractOption(destination map[string]interface{}, options map[string]interface{}, destinationKey string, sourceKeys ...string) {
	for _, key := range sourceKeys {
		if value, ok := options[key]; ok {
			destination[destinationKey] = value
			return
		}
	}
}

func normalizeFirecrawlExtractResult(payload map[string]interface{}, targetURL string, req model.ExtractRequest) (model.ExtractResult, *model.ExtractFailure) {
	if success, ok := payload["success"].(bool); ok && !success {
		return model.ExtractResult{}, &model.ExtractFailure{
			URL:       targetURL,
			Provider:  model.ProviderFirecrawl,
			ErrorType: ErrorTypeUpstream,
			Error:     firstNonEmpty(stringValue(payload, "error", "message"), "Firecrawl could not extract this URL"),
		}
	}
	item := mapFromInterface(payload["data"])
	if item == nil {
		return model.ExtractResult{}, &model.ExtractFailure{
			URL: targetURL, Provider: model.ProviderFirecrawl, ErrorType: ErrorTypeInvalidResponse, Error: "Firecrawl returned no scrape data",
		}
	}
	metadata := mapFromInterface(item["metadata"])
	content, format := firecrawlExtractContent(item, req.Format)
	result := model.ExtractResult{
		// Keep the requested URL so batch fallback can correlate redirects with
		// the original input. Canonical details remain available in Raw.
		URL:       targetURL,
		Content:   content,
		Format:    format,
		Provider:  model.ProviderFirecrawl,
		Providers: []string{model.ProviderFirecrawl},
		Images:    stringArrayValue(item, "images"),
	}
	if metadata != nil {
		result.Title = stringValue(metadata, "title", "ogTitle")
		result.Author = stringValue(metadata, "author")
		result.Favicon = stringValue(metadata, "favicon")
		result.PublishedAt = parseTimeValue(stringValue(metadata, "publishedTime", "publishedDate", "date"))
		if req.IncludeImages && len(result.Images) == 0 {
			if imageURL := stringValue(metadata, "ogImage"); imageURL != "" {
				result.Images = []string{imageURL}
			}
		}
	}
	if req.IncludeRaw {
		result.Raw = item
	}
	return result, nil
}

func firecrawlExtractContent(item map[string]interface{}, requested model.ExtractFormat) (string, model.ExtractFormat) {
	switch requested {
	case model.ExtractFormatHTML:
		if content := stringValue(item, "html"); content != "" {
			return content, model.ExtractFormatHTML
		}
		if content := stringValue(item, "rawHtml"); content != "" {
			return content, model.ExtractFormatRawHTML
		}
	case model.ExtractFormatRawHTML:
		if content := stringValue(item, "rawHtml"); content != "" {
			return content, model.ExtractFormatRawHTML
		}
		if content := stringValue(item, "html"); content != "" {
			return content, model.ExtractFormatHTML
		}
	case model.ExtractFormatText:
		if content := stringValue(item, "text"); content != "" {
			return content, model.ExtractFormatText
		}
		// Firecrawl does not expose a text scrape format. Request markdown and
		// render it here so callers never receive markdown labelled as text.
		if content := stringValue(item, "markdown", "content"); content != "" {
			return markdownToPlainText(content), model.ExtractFormatText
		}
		if content := stringValue(item, "html", "rawHtml"); content != "" {
			return htmlToPlainText(content), model.ExtractFormatText
		}
	}
	if content := stringValue(item, "markdown", "content"); content != "" {
		return content, model.ExtractFormatMarkdown
	}
	if content := stringValue(item, "html"); content != "" {
		return content, model.ExtractFormatHTML
	}
	return stringValue(item, "rawHtml"), model.ExtractFormatRawHTML
}

var (
	markdownATXHeading     = regexp.MustCompile(`^\s{0,3}#{1,6}(?:\s+|$)`)
	markdownClosingHeading = regexp.MustCompile(`\s+#+\s*$`)
	markdownBlockQuote     = regexp.MustCompile(`^(?:\s{0,3}>\s*)+`)
	markdownUnorderedList  = regexp.MustCompile(`^\s{0,3}[-+*]\s+`)
	markdownOrderedList    = regexp.MustCompile(`^\s{0,3}([0-9]{1,9})[.)]\s+`)
	markdownTaskMarker     = regexp.MustCompile(`^\[[ xX]\]\s+`)
	markdownThematicBreak  = regexp.MustCompile(`^\s{0,3}(?:([*_-])\s*){3,}$`)
	markdownSetextHeading  = regexp.MustCompile(`^\s{0,3}(?:=+|-+)\s*$`)
	markdownTableDelimiter = regexp.MustCompile(`^\s*\|?\s*:?-{3,}:?\s*(?:\|\s*:?-{3,}:?\s*)+\|?\s*$`)
	markdownReference      = regexp.MustCompile(`^\s{0,3}\[[^]]+\]:\s+\S+`)
	markdownImage          = regexp.MustCompile(`!\[([^]]*)\]\([^\n)]*\)`)
	markdownInlineLink     = regexp.MustCompile(`\[([^]]+)\]\([^\n)]*\)`)
	markdownReferenceLink  = regexp.MustCompile(`\[([^]]+)\]\[[^]]*\]`)
	markdownAutoLink       = regexp.MustCompile(`<((?:https?://|mailto:)[^>]+)>`)
	markdownEmailAutoLink  = regexp.MustCompile(`<([^ <>]+@[^ <>]+)>`)
	markdownInlineCode     = regexp.MustCompile("`+([^`\\n]+)`+")
	markdownHTMLBlockTag   = regexp.MustCompile(`(?i)</?(?:address|article|aside|blockquote|br|dd|div|dl|dt|figcaption|figure|footer|h[1-6]|header|hr|li|main|nav|ol|p|pre|section|table|tbody|td|tfoot|th|thead|tr|ul)[^>]*>`)
	markdownHTMLTag        = regexp.MustCompile(`(?i)</?(?:a|abbr|audio|b|bdi|bdo|button|canvas|cite|code|data|del|details|dfn|dialog|em|embed|fieldset|form|i|iframe|img|input|ins|kbd|label|legend|map|mark|meter|noscript|object|option|output|picture|progress|q|ruby|s|samp|select|slot|small|source|span|strong|sub|summary|sup|template|textarea|time|track|u|var|video|wbr)(?:\s[^>]*)?/?>`)
	markdownHTMLComment    = regexp.MustCompile(`<!--[^\n]*?-->`)
)

func markdownToPlainText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	lines := strings.Split(value, "\n")
	plain := make([]string, 0, len(lines))
	inFence := false
	lastBlank := true
	for _, rawLine := range lines {
		trimmed := strings.TrimSpace(rawLine)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			plain = append(plain, strings.TrimRight(rawLine, " \t"))
			lastBlank = false
			continue
		}
		if markdownThematicBreak.MatchString(rawLine) || markdownSetextHeading.MatchString(rawLine) || markdownTableDelimiter.MatchString(rawLine) || markdownReference.MatchString(rawLine) {
			continue
		}

		line := markdownBlockQuote.ReplaceAllString(rawLine, "")
		line = markdownATXHeading.ReplaceAllString(line, "")
		line = markdownClosingHeading.ReplaceAllString(line, "")
		if markdownUnorderedList.MatchString(line) {
			line = markdownUnorderedList.ReplaceAllString(line, "• ")
		}
		line = markdownOrderedList.ReplaceAllString(line, "$1. ")
		if strings.HasPrefix(line, "• ") {
			line = "• " + markdownTaskMarker.ReplaceAllString(strings.TrimPrefix(line, "• "), "")
		} else {
			line = markdownTaskMarker.ReplaceAllString(line, "")
		}
		if strings.HasPrefix(strings.TrimSpace(line), "|") && strings.HasSuffix(strings.TrimSpace(line), "|") {
			cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
			for index := range cells {
				cells[index] = strings.TrimSpace(cells[index])
			}
			line = strings.Join(cells, "\t")
		}
		line = markdownInlineToPlainText(line)
		line = strings.TrimSpace(line)
		if line == "" {
			if !lastBlank {
				plain = append(plain, "")
				lastBlank = true
			}
			continue
		}
		plain = append(plain, line)
		lastBlank = false
	}
	return strings.TrimSpace(strings.Join(plain, "\n"))
}

func markdownInlineToPlainText(value string) string {
	value = protectMarkdownEscapes(value)
	value, inlineCode := protectMarkdownInlineCode(value)
	value = markdownImage.ReplaceAllString(value, "$1")
	value = markdownInlineLink.ReplaceAllString(value, "$1")
	value = markdownReferenceLink.ReplaceAllString(value, "$1")
	value = markdownAutoLink.ReplaceAllString(value, "$1")
	value = markdownEmailAutoLink.ReplaceAllString(value, "$1")
	value = stripMarkdownDelimiter(value, "**")
	value = stripMarkdownDelimiter(value, "__")
	value = stripMarkdownDelimiter(value, "*")
	value = stripMarkdownDelimiter(value, "_")
	value = stripMarkdownDelimiter(value, "~~")
	value = markdownHTMLComment.ReplaceAllString(value, "")
	value = markdownHTMLBlockTag.ReplaceAllString(value, "\n")
	value = markdownHTMLTag.ReplaceAllString(value, "")
	value = restoreMarkdownInlineCode(value, inlineCode)
	value = restoreMarkdownEscapes(value)
	return html.UnescapeString(value)
}

const markdownInlineCodeBase rune = 0xF0000

func protectMarkdownInlineCode(value string) (string, []string) {
	matches := markdownInlineCode.FindAllStringSubmatchIndex(value, -1)
	if len(matches) == 0 {
		return value, nil
	}
	var protected strings.Builder
	protected.Grow(len(value))
	code := make([]string, 0, len(matches))
	last := 0
	for _, match := range matches {
		protected.WriteString(value[last:match[0]])
		protected.WriteRune(markdownInlineCodeBase + rune(len(code)))
		code = append(code, value[match[2]:match[3]])
		last = match[1]
	}
	protected.WriteString(value[last:])
	return protected.String(), code
}

func restoreMarkdownInlineCode(value string, code []string) string {
	if len(code) == 0 {
		return value
	}
	var restored strings.Builder
	restored.Grow(len(value))
	for _, character := range value {
		index := int(character - markdownInlineCodeBase)
		if index >= 0 && index < len(code) {
			restored.WriteString(code[index])
			continue
		}
		restored.WriteRune(character)
	}
	return restored.String()
}

func stripMarkdownDelimiter(value, delimiter string) string {
	var plain strings.Builder
	plain.Grow(len(value))
	cursor := 0
	for cursor < len(value) {
		openOffset := strings.Index(value[cursor:], delimiter)
		if openOffset < 0 {
			break
		}
		open := cursor + openOffset
		innerStart := open + len(delimiter)
		closeOffset := strings.Index(value[innerStart:], delimiter)
		if closeOffset < 0 {
			break
		}
		close := innerStart + closeOffset
		inner := value[innerStart:close]
		if validMarkdownDelimiter(value, open, close, len(delimiter), inner) {
			plain.WriteString(value[cursor:open])
			plain.WriteString(inner)
			cursor = close + len(delimiter)
			continue
		}
		plain.WriteString(value[cursor:innerStart])
		cursor = innerStart
	}
	plain.WriteString(value[cursor:])
	return plain.String()
}

func validMarkdownDelimiter(value string, open, close, delimiterLength int, inner string) bool {
	if inner == "" {
		return false
	}
	first, _ := utf8.DecodeRuneInString(inner)
	last, _ := utf8.DecodeLastRuneInString(inner)
	if unicode.IsSpace(first) || unicode.IsSpace(last) {
		return false
	}
	before := rune(0)
	if open > 0 {
		before, _ = utf8.DecodeLastRuneInString(value[:open])
	}
	after := rune(0)
	if close+delimiterLength < len(value) {
		after, _ = utf8.DecodeRuneInString(value[close+delimiterLength:])
	}
	return markdownDelimiterBoundary(before) && markdownDelimiterBoundary(after)
}

func markdownDelimiterBoundary(character rune) bool {
	return character == 0 || unicode.IsSpace(character) || unicode.IsPunct(character) || unicode.IsSymbol(character)
}

func htmlToPlainText(value string) string {
	return markdownToPlainText(markdownHTMLBlockTag.ReplaceAllString(value, "\n"))
}

const markdownEscapable = "\\`*{}[]()#+-.!_>~|"

func protectMarkdownEscapes(value string) string {
	runes := []rune(value)
	var protected strings.Builder
	protected.Grow(len(value))
	for index := 0; index < len(runes); index++ {
		if runes[index] == '\\' && index+1 < len(runes) {
			if escapedIndex := strings.IndexRune(markdownEscapable, runes[index+1]); escapedIndex >= 0 {
				protected.WriteRune(rune(0xE000 + escapedIndex))
				index++
				continue
			}
		}
		protected.WriteRune(runes[index])
	}
	return protected.String()
}

func restoreMarkdownEscapes(value string) string {
	var restored strings.Builder
	restored.Grow(len(value))
	for _, character := range value {
		index := int(character - 0xE000)
		if index >= 0 && index < len([]rune(markdownEscapable)) {
			restored.WriteRune([]rune(markdownEscapable)[index])
			continue
		}
		restored.WriteRune(character)
	}
	return restored.String()
}

func firecrawlTBS(req model.SearchRequest) string {
	if value := optionString(req.Options, "tbs"); value != "" {
		return value
	}
	switch strings.ToLower(strings.TrimSpace(req.Freshness)) {
	case "hour", "qdr:h":
		return "qdr:h"
	case "day", "d", "qdr:d", "pd":
		return "qdr:d"
	case "week", "w", "qdr:w", "pw":
		return "qdr:w"
	case "month", "m", "qdr:m", "pm":
		return "qdr:m"
	case "year", "y", "qdr:y", "py":
		return "qdr:y"
	default:
		return req.Freshness
	}
}

func normalizeFirecrawlResults(payload map[string]interface{}, includeRaw bool) []model.SearchResult {
	items := resultArray(payload, "data")
	if data := mapFromInterface(payload["data"]); data != nil {
		items = append(resultArray(data, "web"), resultArray(data, "news")...)
	}
	results := make([]model.SearchResult, 0, len(items))
	for index, rawItem := range items {
		item := mapFromInterface(rawItem)
		if item == nil {
			continue
		}
		metadata := mapFromInterface(item["metadata"])
		url := stringValue(item, "url")
		if url == "" && metadata != nil {
			url = stringValue(metadata, "sourceURL", "url")
		}
		if url == "" {
			continue
		}
		title := stringValue(item, "title")
		if title == "" && metadata != nil {
			title = stringValue(metadata, "title")
		}
		snippet := stringValue(item, "description", "snippet")
		if snippet == "" && metadata != nil {
			snippet = stringValue(metadata, "description")
		}
		content := stringValue(item, "markdown", "html", "rawHtml", "description", "snippet")
		position := floatValue(item, "position")
		score := 1 / float64(index+1)
		if position > 0 {
			score = 1 / position
		}
		result := model.SearchResult{
			Title:       title,
			URL:         url,
			Snippet:     truncate(snippet, 1000),
			Content:     truncate(content, 4000),
			Provider:    model.ProviderFirecrawl,
			Providers:   []string{model.ProviderFirecrawl},
			Score:       score,
			PublishedAt: parseTimeValue(stringValue(item, "date", "publishedDate", "published_at")),
		}
		if includeRaw {
			result.Raw = item
		}
		results = append(results, result)
	}
	return results
}
