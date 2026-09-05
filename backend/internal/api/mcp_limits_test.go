package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/vihor3/searchmeld/backend/internal/config"
	"github.com/vihor3/searchmeld/backend/internal/model"
	"github.com/vihor3/searchmeld/backend/internal/provider"
	"github.com/vihor3/searchmeld/backend/internal/search"
)

func mcpLimitsServer(h *Handler, bodyLimit int64) *Server {
	server := NewServer(config.Config{RequestBodyLimitBytes: bodyLimit}, &adminAuthTestLogger{})
	server.Mount(func(r chi.Router) {
		h.mountMCP(r, "/mcp")
		h.mountMCP(r, "/v1/mcp")
	})
	return server
}

func TestMountedMCPDiscoveryBatchBounds(t *testing.T) {
	server := mcpLimitsServer(&Handler{}, 1024*1024)
	for _, path := range []string{"/mcp", "/mcp/", "/v1/mcp", "/v1/mcp/"} {
		for _, count := range []int{1, 8, maxMCPBatchRequests, maxMCPBatchRequests + 1} {
			t.Run(fmt.Sprintf("%s/%d", path, count), func(t *testing.T) {
				requests := make([]mcpRequest, count)
				for index := range requests {
					requests[index] = mcpRequest{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprint(index)), Method: "tools/list"}
				}
				body, err := json.Marshal(requests)
				if err != nil {
					t.Fatalf("encode batch: %v", err)
				}
				wantStatus := http.StatusOK
				if count > maxMCPBatchRequests {
					wantStatus = http.StatusBadRequest
				}
				response := adminTestResponse(t, server.Router(), adminTestRequest(http.MethodPost, path, string(body), nil), wantStatus)
				if response.Body.Len() > maxMCPDiscoveryBytes || response.Header().Get("Mcp-Protocol-Version") != mcpLatestProtocolVersion {
					t.Fatal("discovery exceeded its encoded budget or lost its protocol header")
				}
				if count > maxMCPBatchRequests {
					assertMCPBoundError(t, response, -32600)
					return
				}
				var results []struct {
					ID     int `json:"id"`
					Result struct {
						Tools []struct {
							Name string `json:"name"`
						} `json:"tools"`
					} `json:"result"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &results); err != nil || len(results) != count {
					t.Fatalf("discovery batch response count changed: %v", err)
				}
				for index, result := range results {
					if result.ID != index || len(result.Result.Tools) != 2 || result.Result.Tools[0].Name != "search" || result.Result.Tools[1].Name != "extract" {
						t.Fatalf("batch schema contract changed at response %d", index)
					}
				}
			})
		}
	}
}

func TestMountedMCPRejectsOversizedBatchBeforeAuthentication(t *testing.T) {
	server := mcpLimitsServer(&Handler{}, 1024*1024)
	for _, method := range []string{"tools/call", "unknown", "notifications/initialized"} {
		requests := make([]mcpRequest, maxMCPBatchRequests+1)
		for index := range requests {
			requests[index] = mcpRequest{JSONRPC: "2.0", Method: "notifications/initialized"}
		}
		requests[0].Method = method
		body, err := json.Marshal(requests)
		if err != nil {
			t.Fatalf("encode batch: %v", err)
		}
		response := adminTestResponse(t, server.Router(), adminTestRequest(http.MethodPost, "/mcp", string(body), nil), http.StatusBadRequest)
		assertMCPBoundError(t, response, -32600)
	}
	response := adminTestResponse(t, server.Router(), adminTestRequest(http.MethodPost, "/mcp", `[]`, nil), http.StatusBadRequest)
	assertMCPBoundError(t, response, -32600)
}

func TestMountedMCPEncodedDiscoveryBudget(t *testing.T) {
	server := mcpLimitsServer(&Handler{}, 1024*1024)
	for _, count := range []int{1, 8} {
		t.Run(fmt.Sprintf("entries=%d", count), func(t *testing.T) {
			requests := make([]map[string]interface{}, count)
			for index := range requests {
				requests[index] = map[string]interface{}{"jsonrpc": "2.0", "method": "tools/list", "id": strings.Repeat("<", 50000/count)}
			}
			var body bytes.Buffer
			encoder := json.NewEncoder(&body)
			encoder.SetEscapeHTML(false)
			var request interface{} = requests
			if count == 1 {
				request = requests[0]
			}
			if err := encoder.Encode(request); err != nil {
				t.Fatalf("encode request: %v", err)
			}
			if body.Len() >= maxMCPDiscoveryBytes {
				t.Fatal("fixture must fit below the limit before output JSON escaping")
			}
			response := adminTestResponse(t, server.Router(), adminTestRequest(http.MethodPost, "/mcp", body.String(), nil), http.StatusRequestEntityTooLarge)
			assertMCPBoundError(t, response, -32000)
			if response.Body.Len() > 256 || strings.Contains(response.Body.String(), "tools") {
				t.Fatal("overflow returned partial schemas or repeated the oversized ID")
			}
		})
	}
}

func TestMountedMCPStillEnforcesRequestBodyLimit(t *testing.T) {
	server := mcpLimitsServer(&Handler{}, 64)
	body := `{"jsonrpc":"2.0","method":"tools/list","id":"` + strings.Repeat("x", 64) + `"}`
	response := adminTestResponse(t, server.Router(), adminTestRequest(http.MethodPost, "/mcp", body, nil), http.StatusBadRequest)
	assertMCPBoundError(t, response, -32700)
}

func TestMCPNotificationsRemainAcceptedAndOmitted(t *testing.T) {
	server := mcpLimitsServer(&Handler{}, 1024*1024)
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`[{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","method":"tools/list"}]`,
	} {
		response := adminTestResponse(t, server.Router(), adminTestRequest(http.MethodPost, "/mcp", body, nil), http.StatusAccepted)
		if response.Body.Len() != 0 {
			t.Fatal("notification-only request produced a JSON-RPC result")
		}
	}
	body := `[{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":7,"method":"ping"}]`
	response := adminTestResponse(t, server.Router(), adminTestRequest(http.MethodPost, "/mcp", body, nil), http.StatusOK)
	var results []mcpResponse
	if err := json.Unmarshal(response.Body.Bytes(), &results); err != nil || len(results) != 1 || string(results[0].ID) != "7" {
		t.Fatalf("mixed notification contract changed: %s", response.Body.String())
	}
}

func TestMCPResponseEncodingBudgetAndFailure(t *testing.T) {
	response := newMCPResult(json.RawMessage("1"), map[string]interface{}{"text": "<content>"})
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("encode response: %v", err)
	}
	if _, err := marshalMCPResponse(response, len(encoded)); err != nil {
		t.Fatalf("exact encoded boundary rejected: %v", err)
	}
	if _, err := marshalMCPResponse(response, len(encoded)-1); !errors.Is(err, errMCPResponseTooLarge) {
		t.Fatalf("encoded overflow error = %v", err)
	}
	response.Result = make(chan string)
	_, err = marshalMCPResponse(response, 1024)
	if err == nil {
		t.Fatal("invalid response encoding succeeded")
	}
	recorder := httptest.NewRecorder()
	writeMCPEncodingError(recorder, err)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("encoding error HTTP status = %d", recorder.Code)
	}
	assertMCPBoundError(t, recorder, -32603)
	if strings.Contains(recorder.Body.String(), "chan") {
		t.Fatal("encoding failure exposed implementation details")
	}
	id, err := json.Marshal(strings.Repeat("x", maxMCPDiscoveryBytes))
	if err != nil {
		t.Fatalf("encode oversized ID: %v", err)
	}
	recorder = httptest.NewRecorder()
	writeMCPError(recorder, http.StatusUnauthorized, id, -32001, "api token required", nil)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized error response status = %d", recorder.Code)
	}
	assertMCPBoundError(t, recorder, -32000)
}

func TestMountedMCPCancellationStopsBeforeReading(t *testing.T) {
	server := mcpLimitsServer(&Handler{}, 1024*1024)
	body := bytes.NewBufferString(`[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`)
	size := body.Len()
	r := httptest.NewRequest(http.MethodPost, "/mcp", body)
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	response := adminTestResponse(t, server.Router(), r.WithContext(ctx), http.StatusRequestTimeout)
	assertMCPBoundError(t, response, -32000)
	if body.Len() != size {
		t.Fatal("canceled discovery request consumed its body")
	}
}

func TestMountedMCPCancellationDuringBodyRead(t *testing.T) {
	server := mcpLimitsServer(&Handler{}, 1024*1024)
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	r.Body = &cancelingMCPBody{cancel: cancel}
	response := adminTestResponse(t, server.Router(), r.WithContext(ctx), http.StatusRequestTimeout)
	assertMCPBoundError(t, response, -32000)
}

type cancelingMCPBody struct {
	cancel context.CancelFunc
}

func (b *cancelingMCPBody) Read([]byte) (int, error) {
	b.cancel()
	return 0, context.Canceled
}

func (*cancelingMCPBody) Close() error {
	return nil
}

func TestMCPBatchCancellationDiscardsBufferedResults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter := &policyTestProvider{name: model.ProviderBrave, searchHook: cancel}
	store := &policyTestStore{scopeAuthStore: &scopeAuthStore{
		settings: model.RuntimeSettings{APIAuthRequired: true},
		token:    model.APIToken{ID: 42, Scopes: []string{"search"}, AllowedProviders: []string{model.ProviderBrave}},
	}}
	orchestrator := search.NewOrchestrator(provider.NewRegistry(adapter), &policyTestKeyPool{}, store)
	h := NewHandler(store, NewAuthService(store, 0, 0, 0, 0), orchestrator)
	server := mcpLimitsServer(h, 1024*1024)
	body := `[{"jsonrpc":"2.0","id":1,"method":"tools/list"},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"cancel","providers":["brave"]}}},{"jsonrpc":"2.0","id":3,"method":"tools/list"}]`
	r := adminTestRequest(http.MethodPost, "/mcp", body, nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer synthetic-token")
	response := adminTestResponse(t, server.Router(), r, http.StatusRequestTimeout)
	assertMCPBoundError(t, response, -32000)
	if adapter.calls.Load() != 1 || store.usageMarks != 1 || bytes.Contains(response.Body.Bytes(), []byte("inputSchema")) {
		t.Fatal("batch cancellation leaked buffered schemas or changed admitted tool accounting")
	}
}

func TestMCPFullExtractSurvivesDiscoveryBudget(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch=%t", batch), func(t *testing.T) {
			content := strings.Repeat("<", 64*1024) + "FULL-CONTENT-TAIL"
			adapter := &mcpLimitExtractProvider{policyTestProvider: policyTestProvider{name: model.ProviderTavily}, content: content}
			store := &scopeAuthStore{
				settings:      model.RuntimeSettings{APIAuthRequired: true},
				token:         model.APIToken{ID: 42, Scopes: []string{"extract"}, AllowedProviders: []string{model.ProviderTavily}},
				expectedToken: "osr_extract-limit",
			}
			orchestrator := search.NewOrchestrator(provider.NewRegistry(adapter), &policyTestKeyPool{}, mcpLimitExtractStore{})
			h := NewHandler(store, NewAuthService(store, 0, 0, 0, 0), orchestrator)
			server := mcpLimitsServer(h, 1024*1024)
			body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"extract","arguments":{"urls":["https://example.com"],"providers":["tavily"]}}}`
			if batch {
				body = `[{"jsonrpc":"2.0","id":1,"method":"tools/list"},` + body + `,{"jsonrpc":"2.0","method":"notifications/initialized"}]`
			}
			r := adminTestRequest(http.MethodPost, "/mcp", body, nil)
			r.Header.Set("Authorization", "Bearer osr_extract-limit")
			response := adminTestResponse(t, server.Router(), r, http.StatusOK)
			if response.Body.Len() <= maxMCPDiscoveryBytes || response.Body.Len() > maxMCPResponseBytes || store.usageMarks != 1 {
				t.Fatal("fixture did not exercise the tool budget with normal authentication")
			}
			payload := response.Body.Bytes()
			if batch {
				var results []json.RawMessage
				if err := json.Unmarshal(payload, &results); err != nil || len(results) != 2 {
					t.Fatal("single-tool discovery batch lost responses or included a notification")
				}
				payload = results[1]
			}
			var result struct {
				Result struct {
					Content           []mcpContent          `json:"content"`
					StructuredContent model.ExtractResponse `json:"structuredContent"`
					IsError           bool                  `json:"isError"`
				} `json:"result"`
			}
			if err := json.Unmarshal(payload, &result); err != nil {
				t.Fatalf("decode Extract result: %v", err)
			}
			if result.Result.IsError || len(result.Result.StructuredContent.Results) != 1 || result.Result.StructuredContent.Results[0].Content != content {
				t.Fatal("MCP limit truncated the full structured Extract payload")
			}
			if len(result.Result.Content) != 1 || strings.Contains(result.Result.Content[0].Text, "FULL-CONTENT-TAIL") {
				t.Fatal("MCP text no longer uses the bounded Extract preview")
			}
		})
	}
}

func assertMCPBoundError(t *testing.T, response *httptest.ResponseRecorder, code int) {
	t.Helper()
	var result mcpResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Error == nil || result.Error.Code != code || string(result.ID) != "null" {
		t.Fatalf("unexpected bounded MCP error: %s", response.Body.String())
	}
}

type mcpLimitExtractProvider struct {
	policyTestProvider
	content string
}

func (p *mcpLimitExtractProvider) Extract(_ context.Context, req model.ExtractRequest, _ model.APIKey) (model.ExtractProviderResponse, error) {
	return model.ExtractProviderResponse{Results: []model.ExtractResult{{URL: req.URLs[0], Provider: model.ProviderTavily, Content: p.content}}}, nil
}

type mcpLimitExtractStore struct {
	extractValidationStore
}

func (mcpLimitExtractStore) RuntimeSettings(context.Context) (model.RuntimeSettings, error) {
	return model.RuntimeSettings{AllowPrivateExtractTargets: true}, nil
}

func (mcpLimitExtractStore) ListProviders(context.Context) ([]model.ProviderConfig, error) {
	return []model.ProviderConfig{{Name: model.ProviderTavily, Enabled: true, AvailableKeys: 1}}, nil
}
