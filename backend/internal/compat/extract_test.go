package compat

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/one-search/one-search/backend/internal/model"
)

func TestTavilyExtractRequestAcceptsSingleOrMultipleURLs(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want []string
	}{
		{name: "single", body: `{"urls":"https://example.com"}`, want: []string{"https://example.com"}},
		{name: "multiple", body: `{"urls":["https://example.com/a","https://example.com/b"]}`, want: []string{"https://example.com/a", "https://example.com/b"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var request TavilyExtractRequest
			if err := json.Unmarshal([]byte(test.body), &request); err != nil {
				t.Fatalf("unmarshal request: %v", err)
			}
			if !reflect.DeepEqual([]string(request.URLs), test.want) {
				t.Fatalf("urls = %#v, want %#v", request.URLs, test.want)
			}
		})
	}
}

func TestTavilyExtractMapping(t *testing.T) {
	timeout := 12.5
	chunksPerSource := 3
	request := TavilyExtractRequest{
		URLs:            StringList{"https://example.com"},
		Query:           "important details",
		ChunksPerSource: &chunksPerSource,
		ExtractDepth:    "advanced",
		IncludeImages:   true,
		IncludeFavicon:  true,
		Format:          "text",
		Timeout:         &timeout,
		IncludeUsage:    true,
		Providers:       []string{model.ProviderTavily},
		Mode:            string(model.SearchModeSingle),
	}
	native := TavilyExtractToNative(request)
	if !reflect.DeepEqual(native.URLs, []string{"https://example.com"}) || native.Query != request.Query {
		t.Fatalf("unexpected native request: %+v", native)
	}
	if native.Format != model.ExtractFormatText || native.CompatFormat != model.CompatFormatTavily {
		t.Fatalf("unexpected native formats: %+v", native)
	}
	if native.ChunksPerSource != 3 || !native.ChunksPerSourceSet {
		t.Fatalf("unexpected chunks mapping: %+v", native)
	}
	if native.Options["timeout"] != 12.5 {
		t.Fatalf("timeout option = %#v", native.Options["timeout"])
	}

	response := TavilyExtractFromNative(request, model.ExtractResponse{
		Results: []model.ExtractResult{{
			URL:     "https://example.com",
			Content: "page content",
			Images:  []string{"https://example.com/image.png"},
			Favicon: "https://example.com/favicon.ico",
		}},
		FailedResults: []model.ExtractFailure{{URL: "https://failed.example", Error: "blocked"}},
		Usage:         []model.UsageMeasurement{{Unit: "credits", Quantity: 2}, {Unit: "usd", Quantity: 0.01}},
		Meta:          model.ExtractMetadata{RequestID: "req-1", LatencyMS: 1250},
	})
	if response.RequestID != "req-1" || response.ResponseTime != 1.25 || len(response.Results) != 1 || len(response.FailedResults) != 1 {
		t.Fatalf("unexpected Tavily response: %+v", response)
	}
	if response.Results[0].RawContent != "page content" || response.FailedResults[0].Error != "blocked" {
		t.Fatalf("unexpected Tavily result mapping: %+v", response)
	}
	if response.Usage == nil || response.Usage.Credits != 2 {
		t.Fatalf("unexpected Tavily usage: %+v", response.Usage)
	}
}
