package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/one-search/one-search/backend/internal/model"
	"github.com/one-search/one-search/backend/internal/provider"
	"github.com/one-search/one-search/backend/internal/search"
)

func TestMCPStreamableHTTPHandshakeAndListTools(t *testing.T) {
	h := &Handler{}
	r := chi.NewRouter()
	h.mountMCP(r, "/mcp")

	post := func(payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	initRec := post(`{
		"jsonrpc":"2.0",
		"id":1,
		"method":"initialize",
		"params":{
			"protocolVersion":"2025-06-18",
			"capabilities":{},
			"clientInfo":{"name":"test-client","version":"1.0.0"}
		}
	}`)
	if initRec.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, body = %s", initRec.Code, initRec.Body.String())
	}
	var initResp struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools map[string]interface{} `json:"tools"`
			} `json:"capabilities"`
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(initRec.Body.Bytes(), &initResp); err != nil {
		t.Fatalf("decode initialize response: %v", err)
	}
	if initResp.JSONRPC != "2.0" || initResp.ID != 1 || initResp.Result.ProtocolVersion != "2025-06-18" {
		t.Fatalf("unexpected initialize response: %+v", initResp)
	}
	if initResp.Result.ServerInfo.Name != "one-search-relay" || initResp.Result.ServerInfo.Version == "" {
		t.Fatalf("unexpected serverInfo: %+v", initResp.Result.ServerInfo)
	}
	if initResp.Result.Capabilities.Tools == nil {
		t.Fatalf("tools capability missing: %+v", initResp.Result.Capabilities)
	}

	initializedRec := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if initializedRec.Code != http.StatusAccepted {
		t.Fatalf("initialized status = %d, body = %s", initializedRec.Code, initializedRec.Body.String())
	}

	toolsRec := post(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if toolsRec.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d, body = %s", toolsRec.Code, toolsRec.Body.String())
	}
	var toolsResp struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			Tools []struct {
				Name        string                 `json:"name"`
				Description string                 `json:"description"`
				InputSchema map[string]interface{} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(toolsRec.Body.Bytes(), &toolsResp); err != nil {
		t.Fatalf("decode tools/list response: %v", err)
	}
	if toolsResp.JSONRPC != "2.0" || toolsResp.ID != 2 || len(toolsResp.Result.Tools) != 2 {
		t.Fatalf("unexpected tools/list response: %+v", toolsResp)
	}
	searchTool := toolsResp.Result.Tools[0]
	if searchTool.Name != "search" || searchTool.Description == "" || searchTool.InputSchema["type"] != "object" {
		t.Fatalf("unexpected search tool schema: %+v", searchTool)
	}
	properties, ok := searchTool.InputSchema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("search tool properties missing: %+v", searchTool.InputSchema)
	}
	providers, ok := properties["providers"].(map[string]interface{})
	if !ok {
		t.Fatalf("providers schema missing: %+v", properties)
	}
	items, ok := providers["items"].(map[string]interface{})
	if !ok {
		t.Fatalf("providers items schema missing: %+v", providers)
	}
	enumValues, ok := items["enum"].([]interface{})
	if !ok || len(enumValues) != len(model.DefaultProviders) {
		t.Fatalf("unexpected providers enum: %+v", items["enum"])
	}
	for index, provider := range model.DefaultProviders {
		if enumValues[index] != provider {
			t.Fatalf("providers enum[%d] = %v, want %s", index, enumValues[index], provider)
		}
	}

	extractTool := toolsResp.Result.Tools[1]
	if extractTool.Name != "extract" || extractTool.Description == "" || extractTool.InputSchema["type"] != "object" {
		t.Fatalf("unexpected extract tool schema: %+v", extractTool)
	}
	extractProperties, ok := extractTool.InputSchema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("extract tool properties missing: %+v", extractTool.InputSchema)
	}
	urls, ok := extractProperties["urls"].(map[string]interface{})
	if !ok || urls["minItems"] != float64(1) && urls["minItems"] != 1 || urls["maxItems"] != float64(20) && urls["maxItems"] != 20 {
		t.Fatalf("unexpected urls schema: %+v", urls)
	}
	extractProviders, ok := extractProperties["providers"].(map[string]interface{})
	if !ok {
		t.Fatalf("extract providers schema missing: %+v", extractProperties)
	}
	extractItems, ok := extractProviders["items"].(map[string]interface{})
	if !ok {
		t.Fatalf("extract providers items schema missing: %+v", extractProviders)
	}
	extractEnum, ok := extractItems["enum"].([]interface{})
	if !ok || len(extractEnum) != len(model.ExtractProviders) {
		t.Fatalf("unexpected extract providers enum: %+v", extractItems["enum"])
	}
	for index, provider := range model.ExtractProviders {
		if extractEnum[index] != provider {
			t.Fatalf("extract providers enum[%d] = %v, want %s", index, extractEnum[index], provider)
		}
	}
}

func TestMCPBatchRejectsMultipleToolCalls(t *testing.T) {
	h := &Handler{}
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`[
		{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"query":"one"}}},
		{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"extract","arguments":{"urls":["https://example.com"]}}}
	]`))
	response := httptest.NewRecorder()
	h.mcp(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "at most one tools/call") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestValidateExtractRequest(t *testing.T) {
	if err := validateExtractRequest(model.ExtractRequest{}); err == nil {
		t.Fatal("expected missing urls to fail")
	}
	if err := validateExtractRequest(model.ExtractRequest{URLs: []string{" "}}); err == nil {
		t.Fatal("expected blank url to fail")
	}
	urls := make([]string, 21)
	for index := range urls {
		urls[index] = "https://example.com"
	}
	if err := validateExtractRequest(model.ExtractRequest{URLs: urls}); err == nil {
		t.Fatal("expected more than 20 urls to fail")
	}
	if err := validateExtractRequest(model.ExtractRequest{URLs: []string{"https://example.com"}}); err != nil {
		t.Fatalf("valid extract request failed: %v", err)
	}
}

func TestExtractValidationErrorsMapToHTTPBadRequest(t *testing.T) {
	h := extractValidationHandler()
	tests := []struct {
		name    string
		request model.ExtractRequest
	}{
		{name: "invalid URL", request: model.ExtractRequest{URLs: []string{"ftp://example.com"}}},
		{name: "invalid mode", request: model.ExtractRequest{URLs: []string{"https://example.com"}, Mode: "invalid"}},
		{name: "invalid format", request: model.ExtractRequest{URLs: []string{"https://example.com"}, Format: "invalid"}},
		{name: "invalid depth", request: model.ExtractRequest{URLs: []string{"https://example.com"}, ExtractDepth: "invalid"}},
		{name: "invalid chunks", request: model.ExtractRequest{URLs: []string{"https://example.com"}, ChunksPerSource: 6}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/extract", nil)
			rec := httptest.NewRecorder()
			h.runExtract(rec, req, test.request)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestExtractErrorStatus(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{err: context.DeadlineExceeded, want: http.StatusGatewayTimeout},
		{err: &provider.Error{Type: provider.ErrorTypeRateLimited}, want: http.StatusTooManyRequests},
		{err: &provider.Error{Type: provider.ErrorTypeQuotaExhausted}, want: http.StatusTooManyRequests},
		{err: &provider.Error{Type: provider.ErrorTypeNoKey}, want: http.StatusServiceUnavailable},
		{err: &provider.Error{Type: provider.ErrorTypeAuth}, want: http.StatusBadGateway},
	}
	for _, test := range tests {
		if got := extractErrorStatus(test.err); got != test.want {
			t.Fatalf("extractErrorStatus(%v) = %d, want %d", test.err, got, test.want)
		}
	}
}

func TestMCPExtractValidationErrorIsInvalidParams(t *testing.T) {
	h := extractValidationHandler()
	result, errorResponse := h.handleMCPExtractTool(
		httptest.NewRequest(http.MethodPost, "/mcp", nil),
		json.RawMessage("1"),
		json.RawMessage(`{"urls":["ftp://example.com"]}`),
	)
	if result != nil || errorResponse == nil || errorResponse.Error == nil {
		t.Fatalf("unexpected response: result=%+v error=%+v", result, errorResponse)
	}
	if errorResponse.Error.Code != -32602 {
		t.Fatalf("error code = %d, want -32602", errorResponse.Error.Code)
	}
}

func TestMCPToolsEnforceTokenScopes(t *testing.T) {
	h := extractValidationHandler()

	extractRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	extractRequest = extractRequest.WithContext(context.WithValue(extractRequest.Context(), apiTokenKey, model.APIToken{Scopes: []string{"search"}}))
	result, errorResponse := h.handleMCPExtractTool(
		extractRequest,
		json.RawMessage("1"),
		json.RawMessage(`{"urls":["https://example.com"]}`),
	)
	if result != nil || errorResponse == nil || errorResponse.Error == nil || errorResponse.Error.Code != -32003 {
		t.Fatalf("extract scope response: result=%+v error=%+v", result, errorResponse)
	}

	searchRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	searchRequest = searchRequest.WithContext(context.WithValue(searchRequest.Context(), apiTokenKey, model.APIToken{Scopes: []string{"extract"}}))
	result, errorResponse = h.handleMCPSearchTool(searchRequest, json.RawMessage("2"), json.RawMessage(`{"query":"test"}`))
	if result != nil || errorResponse == nil || errorResponse.Error == nil || errorResponse.Error.Code != -32003 {
		t.Fatalf("search scope response: result=%+v error=%+v", result, errorResponse)
	}
}

func TestAPITokenHasScope(t *testing.T) {
	if !apiTokenHasScope(model.APIToken{Scopes: []string{" Search "}}, "search") {
		t.Fatal("expected case-insensitive search scope match")
	}
	if !apiTokenHasScope(model.APIToken{Scopes: []string{"*"}}, "extract") {
		t.Fatal("expected wildcard scope match")
	}
	if apiTokenHasScope(model.APIToken{Scopes: []string{"search"}}, "extract") {
		t.Fatal("search-only token must not receive extract scope")
	}
}

func TestMCPExtractTextKeepsFullContentOnlyInStructuredResponse(t *testing.T) {
	tail := "FULL-CONTENT-TAIL-MUST-NOT-BE-DUPLICATED"
	response := model.ExtractResponse{
		Results: []model.ExtractResult{{
			URL:      "https://example.com",
			Title:    "Example",
			Provider: model.ProviderTavily,
			Content:  strings.Repeat("正文", 1000) + tail,
		}},
		Meta: model.ExtractMetadata{RequestID: "req-1"},
	}
	preview := mcpExtractText(response)
	if strings.Contains(preview, tail) {
		t.Fatal("MCP text content duplicated the full extracted page")
	}
	if len([]rune(preview)) > maxMCPExtractTextRunes {
		t.Fatalf("preview length = %d, max = %d", len([]rune(preview)), maxMCPExtractTextRunes)
	}
	if !strings.Contains(preview, "structuredContent") || !strings.Contains(preview, "https://example.com") {
		t.Fatalf("preview is missing useful summary: %q", preview)
	}
}

func TestMCPExtractMarksAllFailedResponseAsToolError(t *testing.T) {
	result := mcpExtractToolResult(model.ExtractResponse{
		FailedResults: []model.ExtractFailure{{URL: "https://example.com", Error: "blocked"}},
	})
	if result["isError"] != true {
		t.Fatalf("isError = %#v, want true", result["isError"])
	}
}

func TestApplyTokenExtractProvidersFiltersSearchOnlyDefaults(t *testing.T) {
	providers, err := applyTokenExtractProviders(nil, []string{model.ProviderBrave, model.ProviderTavily})
	if err != nil {
		t.Fatalf("filter providers: %v", err)
	}
	if len(providers) != 1 || providers[0] != model.ProviderTavily {
		t.Fatalf("providers = %#v, want tavily", providers)
	}
	if _, err := applyTokenExtractProviders(nil, []string{model.ProviderBrave}); err == nil {
		t.Fatal("expected search-only token provider set to fail")
	}
}

func extractValidationHandler() *Handler {
	orchestrator := search.NewOrchestrator(provider.NewRegistry(), nil, extractValidationStore{})
	return &Handler{orchestrator: orchestrator}
}

type extractValidationStore struct{}

func (extractValidationStore) GetAPIKeyByID(context.Context, int64) (model.APIKey, error) {
	return model.APIKey{}, nil
}

func (extractValidationStore) RecordKeyResult(context.Context, model.APIKey, bool, string) error {
	return nil
}

func (extractValidationStore) UpdateProviderKeyOfficialQuota(context.Context, int64, model.ProviderKeyQuotaResult) error {
	return nil
}

func (extractValidationStore) RuntimeSettings(context.Context) (model.RuntimeSettings, error) {
	return model.RuntimeSettings{}, nil
}

func (extractValidationStore) ListProviders(context.Context) ([]model.ProviderConfig, error) {
	return nil, nil
}

func (extractValidationStore) RecordSearchLog(context.Context, model.SearchLogInput) error {
	return nil
}

func (extractValidationStore) GetCache(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}

func (extractValidationStore) SetCache(context.Context, string, []byte, int) error {
	return nil
}

func TestMCPStreamableHTTPGetSSEIsNotOffered(t *testing.T) {
	h := &Handler{}
	r := chi.NewRouter()
	h.mountMCP(r, "/mcp")

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET SSE status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
