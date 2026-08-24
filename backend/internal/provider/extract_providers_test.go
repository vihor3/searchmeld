package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/one-search/one-search/backend/internal/model"
)

func TestExaProviderExtract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/contents" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "exa-key" {
			t.Fatalf("x-api-key = %q", got)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		urls, _ := body["urls"].([]interface{})
		highlights, _ := body["highlights"].(map[string]interface{})
		if len(urls) != 2 || urls[0] != "https://example.com/a" || highlights["query"] != "important section" {
			t.Fatalf("unexpected request body: %#v", body)
		}
		writeJSON(t, w, map[string]interface{}{
			"costDollars": map[string]interface{}{"total": 0.02},
			"results": []map[string]interface{}{
				{
					"id":            "https://example.com/a",
					"url":           "https://cdn.example.com/redirected-a.pdf",
					"title":         "Example A",
					"text":          "Extracted A",
					"highlights":    []string{"**Important** [A](https://example.com/a)"},
					"image":         "https://example.com/a.png",
					"author":        "Author",
					"publishedDate": "2026-01-02",
				},
			},
			"statuses": []map[string]interface{}{
				{"id": "https://example.com/b", "url": "https://cdn.example.com/redirected-b.pdf", "status": "error", "error": map[string]interface{}{"tag": "CRAWL_NOT_FOUND"}},
			},
		})
	}))
	defer server.Close()

	adapter := NewExaProvider(Config{BaseURL: server.URL})
	response, err := adapter.Extract(context.Background(), model.ExtractRequest{
		URLs:          []string{"https://example.com/a", "https://example.com/b"},
		Query:         "important section",
		Format:        model.ExtractFormatText,
		IncludeImages: true,
		IncludeRaw:    true,
	}, model.APIKey{Value: "exa-key"})
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if len(response.Results) != 1 || response.Results[0].URL != "https://example.com/a" || response.Results[0].Content != "Important A" || response.Results[0].Provider != model.ProviderExa || len(response.Results[0].Images) != 1 || response.Results[0].Raw == nil {
		t.Fatalf("unexpected results: %#v", response.Results)
	}
	if response.Results[0].PublishedAt == nil || len(response.FailedResults) != 1 || response.FailedResults[0].URL != "https://example.com/b" || response.FailedResults[0].Error != "CRAWL_NOT_FOUND" {
		t.Fatalf("unexpected extraction response: %#v", response)
	}
	if len(response.Usage) != 1 || response.Usage[0].Unit != "usd" || response.Usage[0].Quantity != 0.02 {
		t.Fatalf("unexpected usage: %#v", response.Usage)
	}
}

func TestExaProviderExtractConvertsMarkdownTextWithoutQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/contents" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if _, ok := body["highlights"]; ok {
			t.Fatalf("highlights should be omitted without a query: %#v", body)
		}
		writeJSON(t, w, map[string]interface{}{
			"results": []map[string]interface{}{
				{
					"id":   "https://example.com/article",
					"text": "# Article\n\nA **bold** [link](https://example.com).",
				},
			},
		})
	}))
	defer server.Close()

	adapter := NewExaProvider(Config{BaseURL: server.URL})
	response, err := adapter.Extract(context.Background(), model.ExtractRequest{
		URLs:   []string{"https://example.com/article"},
		Format: model.ExtractFormatText,
	}, model.APIKey{Value: "exa-key"})
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("unexpected results: %#v", response.Results)
	}
	result := response.Results[0]
	if result.Content != "Article\n\nA bold link." || result.Format != model.ExtractFormatText {
		t.Fatalf("unexpected text result: %#v", result)
	}
}

func TestTavilyProviderExtract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/extract" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tavily-key" {
			t.Fatalf("Authorization = %q", got)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body["format"] != "text" || body["extract_depth"] != "advanced" || body["chunks_per_source"] != float64(3) || body["include_images"] != true || body["include_favicon"] != true || body["timeout"] != 12.5 {
			t.Fatalf("unexpected request body: %#v", body)
		}
		writeJSON(t, w, map[string]interface{}{
			"usage": map[string]interface{}{"credits": 2},
			"results": []map[string]interface{}{
				{"url": "https://example.com/a", "raw_content": "Tavily content", "images": []string{"https://example.com/a.png"}, "favicon": "https://example.com/favicon.ico"},
			},
			"failed_results": []map[string]interface{}{
				{"url": "https://example.com/b", "error": "blocked"},
			},
		})
	}))
	defer server.Close()

	adapter := NewTavilyProvider(Config{BaseURL: server.URL})
	response, err := adapter.Extract(context.Background(), model.ExtractRequest{
		URLs:            []string{"https://example.com/a", "https://example.com/b"},
		Format:          model.ExtractFormatText,
		ExtractDepth:    "advanced",
		ChunksPerSource: 3,
		IncludeImages:   true,
		IncludeFavicon:  true,
		Options:         map[string]interface{}{"timeout": 12.5},
	}, model.APIKey{Value: "tavily-key"})
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if len(response.Results) != 1 || response.Results[0].Format != model.ExtractFormatText || len(response.Results[0].Images) != 1 || len(response.FailedResults) != 1 {
		t.Fatalf("unexpected extraction response: %#v", response)
	}
	if len(response.Usage) != 1 || response.Usage[0].Quantity != 2 {
		t.Fatalf("unexpected usage: %#v", response.Usage)
	}
}

func TestFirecrawlProviderExtract(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.Method != http.MethodPost || r.URL.Path != "/v2/scrape" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer firecrawl-key" {
			t.Fatalf("Authorization = %q", got)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		formats, _ := body["formats"].([]interface{})
		if len(formats) != 2 || formats[0] != "rawHtml" || formats[1] != "images" || body["onlyMainContent"] != true {
			t.Fatalf("unexpected request body: %#v", body)
		}
		if requestCount == 2 {
			writeJSON(t, w, map[string]interface{}{"success": false, "error": "robots denied"})
			return
		}
		writeJSON(t, w, map[string]interface{}{
			"success":     true,
			"creditsUsed": 1,
			"data": map[string]interface{}{
				"rawHtml": "<main>Firecrawl content</main>",
				"metadata": map[string]interface{}{
					"sourceURL": "https://www.example.com/canonical-a", "title": "Example A", "author": "Author", "favicon": "https://example.com/favicon.ico", "ogImage": "https://example.com/a.png",
				},
			},
		})
	}))
	defer server.Close()

	adapter := NewFirecrawlProvider(Config{BaseURL: server.URL})
	response, err := adapter.Extract(context.Background(), model.ExtractRequest{
		URLs:          []string{"https://example.com/a", "https://example.com/b"},
		Format:        model.ExtractFormatRawHTML,
		IncludeImages: true,
		Options:       map[string]interface{}{"only_main_content": true},
	}, model.APIKey{Value: "firecrawl-key"})
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if requestCount != 2 || len(response.Results) != 1 || response.Results[0].URL != "https://example.com/a" || response.Results[0].Content != "<main>Firecrawl content</main>" || response.Results[0].Format != model.ExtractFormatRawHTML {
		t.Fatalf("unexpected results: %#v", response.Results)
	}
	if len(response.Results[0].Images) != 1 || len(response.FailedResults) != 1 || len(response.Usage) != 1 {
		t.Fatalf("unexpected extraction response: %#v", response)
	}
}

func TestFirecrawlProviderExtractConvertsTavilyTextAndTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/scrape" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		formats, _ := body["formats"].([]interface{})
		if len(formats) != 1 || formats[0] != "markdown" {
			t.Fatalf("formats = %#v, want markdown", formats)
		}
		if body["timeout"] != float64(12501) {
			t.Fatalf("timeout = %#v, want 12501 integer milliseconds", body["timeout"])
		}
		writeJSON(t, w, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"markdown": "# Firecrawl title\n\nA **bold** [link](https://example.com) &amp; `code`.\n\n- First item\n- Second item",
			},
		})
	}))
	defer server.Close()

	adapter := NewFirecrawlProvider(Config{BaseURL: server.URL})
	response, err := adapter.Extract(context.Background(), model.ExtractRequest{
		URLs:         []string{"https://example.com/article"},
		Format:       model.ExtractFormatText,
		CompatFormat: model.CompatFormatTavily,
		Options:      map[string]interface{}{"timeout": 12.5004},
	}, model.APIKey{Value: "firecrawl-key"})
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("unexpected results: %#v", response.Results)
	}
	result := response.Results[0]
	if result.Format != model.ExtractFormatText {
		t.Fatalf("format = %q, want text", result.Format)
	}
	want := "Firecrawl title\n\nA bold link & code.\n\n• First item\n• Second item"
	if result.Content != want {
		t.Fatalf("content = %q, want %q", result.Content, want)
	}
}

func TestFirecrawlExtractBodyPreservesNativeTimeoutMilliseconds(t *testing.T) {
	body := firecrawlExtractBody("https://example.com/article", model.ExtractRequest{
		Format:       model.ExtractFormatMarkdown,
		CompatFormat: model.CompatFormatNative,
		Options:      map[string]interface{}{"timeout": 45000},
	})
	if body["timeout"] != 45000 {
		t.Fatalf("timeout = %#v, want native 45000 milliseconds unchanged", body["timeout"])
	}
}

func TestMarkdownToPlainTextPreservesLiteralSyntax(t *testing.T) {
	markdown := "some_variable_name\n\n2 * 3 * 4\n\n<name@example.com>\n\n`<span>literal</span>` and _italic_"
	want := "some_variable_name\n\n2 * 3 * 4\n\nname@example.com\n\n<span>literal</span> and italic"
	if got := markdownToPlainText(markdown); got != want {
		t.Fatalf("markdownToPlainText() = %q, want %q", got, want)
	}
}

func TestJinaProviderExtract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/https://example.com/article" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer jina-key" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Return-Format"); got != "markdown" {
			t.Fatalf("X-Return-Format = %q", got)
		}
		writeJSON(t, w, map[string]interface{}{
			"data": map[string]interface{}{
				"url": "https://www.example.com/canonical-article", "title": "Article", "content": "# Jina content", "author": "Writer", "images": []string{"https://example.com/article.png"},
			},
		})
	}))
	defer server.Close()

	adapter := NewJinaProvider(Config{BaseURL: "https://s.jina.ai", ExtractBaseURL: server.URL})
	response, err := adapter.Extract(context.Background(), model.ExtractRequest{
		URLs:          []string{"https://example.com/article"},
		Format:        model.ExtractFormatMarkdown,
		IncludeImages: true,
	}, model.APIKey{Value: "jina-key"})
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if len(response.Results) != 1 || response.Results[0].URL != "https://example.com/article" || response.Results[0].Content != "# Jina content" || response.Results[0].Provider != model.ProviderJina || len(response.Results[0].Images) != 1 {
		t.Fatalf("unexpected results: %#v", response.Results)
	}
}

func TestJinaProviderReportsActualHTMLFormat(t *testing.T) {
	if got := jinaExtractFormat(model.ExtractFormatRawHTML); got != model.ExtractFormatHTML {
		t.Fatalf("raw_html request format = %q, want html", got)
	}
}

func TestFirecrawlProviderExtractReturnsAuthError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	adapter := NewFirecrawlProvider(Config{BaseURL: server.URL})
	_, err := adapter.Extract(context.Background(), model.ExtractRequest{
		URLs: []string{"https://example.com/article"},
	}, model.APIKey{Value: "bad-key"})
	if err == nil || ErrorType(err) != ErrorTypeAuth {
		t.Fatalf("Extract error = %v, want auth error", err)
	}
}
