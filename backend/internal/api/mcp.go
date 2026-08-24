package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/one-search/one-search/backend/internal/model"
)

const (
	mcpLatestProtocolVersion  = "2025-06-18"
	mcpDefaultProtocolVersion = "2025-03-26"
	maxMCPExtractTextRunes    = 16 * 1024
	maxMCPExtractPreviewRunes = 512
)

var mcpSupportedProtocolVersions = []string{mcpLatestProtocolVersion, mcpDefaultProtocolVersion, "2024-11-05"}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (h *Handler) mountMCP(r chi.Router, path string) {
	h.mountMCPPath(r, path)
	if strings.HasSuffix(path, "/") {
		trimmed := strings.TrimRight(path, "/")
		if trimmed != "" {
			h.mountMCPPath(r, trimmed)
		}
		return
	}
	h.mountMCPPath(r, path+"/")
}

func (h *Handler) mountMCPPath(r chi.Router, path string) {
	r.Get(path, h.mcpInfo)
	r.Post(path, h.mcp)
	r.Delete(path, h.mcpDelete)
}

func (h *Handler) mcpInfo(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":                     true,
		"transport":                   "streamable-http",
		"protocol_version":            mcpLatestProtocolVersion,
		"supported_protocol_versions": mcpSupportedProtocolVersions,
		"endpoint":                    r.URL.Path,
		"auth":                        "Authorization: Bearer <osr_...|oak_...> or X-API-Key",
		"tools":                       []string{"search", "extract"},
	})
}

func (h *Handler) mcpDelete(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (h *Handler) mcp(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeMCPError(w, http.StatusBadRequest, nil, -32700, "invalid body", nil)
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		writeMCPError(w, http.StatusBadRequest, nil, -32700, "empty json-rpc body", nil)
		return
	}

	if trimmed[0] == '[' {
		var requests []mcpRequest
		if err := json.Unmarshal(trimmed, &requests); err != nil {
			writeMCPError(w, http.StatusBadRequest, nil, -32700, "parse error", err.Error())
			return
		}
		if countMCPToolCalls(requests) > 1 {
			writeMCPError(w, http.StatusBadRequest, firstMCPRequestID(trimmed), -32600, "a JSON-RPC batch may contain at most one tools/call", nil)
			return
		}
		if h.mcpRequestsRequireAuth(requests) {
			ctx, authStatus, authMessage, err := h.mcpAuthContext(r)
			if err != nil {
				writeMCPError(w, authStatus, firstMCPRequestID(trimmed), -32001, authMessage, nil)
				return
			}
			r = r.WithContext(ctx)
		}
		h.handleMCPBatch(w, r, requests)
		return
	}

	var req mcpRequest
	if err := json.Unmarshal(trimmed, &req); err != nil {
		writeMCPError(w, http.StatusBadRequest, nil, -32700, "parse error", err.Error())
		return
	}
	if h.mcpRequestsRequireAuth([]mcpRequest{req}) {
		ctx, authStatus, authMessage, err := h.mcpAuthContext(r)
		if err != nil {
			writeMCPError(w, authStatus, req.ID, -32001, authMessage, nil)
			return
		}
		r = r.WithContext(ctx)
	}
	response, ok := h.handleMCPRequest(r, req)
	if !ok {
		writeMCPAccepted(w)
		return
	}
	writeMCPResponse(w, http.StatusOK, response)
}

func countMCPToolCalls(requests []mcpRequest) int {
	count := 0
	for _, request := range requests {
		if request.Method == "tools/call" {
			count++
		}
	}
	return count
}

func (h *Handler) handleMCPBatch(w http.ResponseWriter, r *http.Request, requests []mcpRequest) {
	if len(requests) == 0 {
		writeMCPError(w, http.StatusBadRequest, nil, -32600, "empty batch is not allowed", nil)
		return
	}
	responses := make([]mcpResponse, 0, len(requests))
	for _, req := range requests {
		response, ok := h.handleMCPRequest(r, req)
		if ok {
			responses = append(responses, response)
		}
	}
	if len(responses) == 0 {
		writeMCPAccepted(w)
		return
	}
	writeMCPBatchResponse(w, http.StatusOK, responses)
}

func (h *Handler) handleMCPRequest(r *http.Request, req mcpRequest) (mcpResponse, bool) {
	if req.ID == nil {
		h.handleMCPNotification(r, req)
		return mcpResponse{}, false
	}
	if req.JSONRPC != "2.0" {
		return newMCPError(req.ID, -32600, "jsonrpc must be 2.0", nil), true
	}
	if req.Method == "" {
		return newMCPError(req.ID, -32600, "method is required", nil), true
	}

	switch req.Method {
	case "initialize":
		return newMCPResult(req.ID, mcpInitializeResult(req.Params)), true
	case "ping":
		return newMCPResult(req.ID, map[string]interface{}{}), true
	case "tools/list":
		return newMCPResult(req.ID, map[string]interface{}{"tools": []interface{}{mcpSearchToolSchema(), mcpExtractToolSchema()}}), true
	case "tools/call":
		result, errResp := h.handleMCPToolCall(r, req)
		if errResp != nil {
			return *errResp, true
		}
		return newMCPResult(req.ID, result), true
	case "resources/list", "prompts/list":
		key := "resources"
		if req.Method == "prompts/list" {
			key = "prompts"
		}
		return newMCPResult(req.ID, map[string]interface{}{key: []interface{}{}}), true
	case "resources/templates/list":
		return newMCPResult(req.ID, map[string]interface{}{"resourceTemplates": []interface{}{}}), true
	default:
		return newMCPError(req.ID, -32601, "method not found", req.Method), true
	}
}

func (h *Handler) handleMCPNotification(r *http.Request, req mcpRequest) {
	_ = r
	_ = req
}

func (h *Handler) handleMCPToolCall(r *http.Request, req mcpRequest) (interface{}, *mcpResponse) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(req.Params) == 0 {
		return nil, mcpInvalidParams(req.ID, "params are required")
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, mcpInvalidParams(req.ID, "invalid params")
	}
	switch params.Name {
	case "search":
		return h.handleMCPSearchTool(r, req.ID, params.Arguments)
	case "extract":
		return h.handleMCPExtractTool(r, req.ID, params.Arguments)
	default:
		return nil, mcpInvalidParams(req.ID, "unknown tool: "+params.Name)
	}
}

func (h *Handler) handleMCPSearchTool(r *http.Request, requestID json.RawMessage, arguments json.RawMessage) (interface{}, *mcpResponse) {
	var searchReq model.SearchRequest
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &searchReq); err != nil {
			return nil, mcpInvalidParams(requestID, "invalid search arguments")
		}
	}
	searchReq.Query = strings.TrimSpace(searchReq.Query)
	if searchReq.Query == "" {
		return nil, mcpInvalidParams(requestID, "query is required")
	}
	searchReq.LimitExplicit = hasJSONField(arguments, "limit")
	searchReq.ProvidersExplicit = hasJSONField(arguments, "providers")
	searchReq.CompatFormat = model.CompatFormatNative
	if searchReq.Options == nil {
		searchReq.Options = map[string]interface{}{}
	}
	searchReq.Options["source"] = "mcp"

	if token, ok := APIToken(r.Context()); ok {
		if !apiTokenHasScope(token, "search") {
			return nil, &mcpResponse{JSONRPC: "2.0", ID: requestID, Error: &mcpError{Code: -32003, Message: "api token does not include search scope"}}
		}
		filtered, err := applyTokenProviders(searchReq.Providers, token.AllowedProviders)
		if err != nil {
			return nil, &mcpResponse{JSONRPC: "2.0", ID: requestID, Error: &mcpError{Code: -32003, Message: err.Error()}}
		}
		searchReq.Providers = filtered
	}

	response, err := h.orchestrator.Search(r.Context(), searchReq, mcpInvocationRequestID(r), APITokenID(r.Context()))
	if err != nil {
		return mcpToolError(err.Error()), nil
	}
	payload, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return mcpToolError(err.Error()), nil
	}
	return map[string]interface{}{
		"content":           []mcpContent{{Type: "text", Text: string(payload)}},
		"structuredContent": response,
		"isError":           false,
	}, nil
}

func mcpExtractText(response model.ExtractResponse) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Extract completed: %d succeeded, %d failed. Full page content is available in structuredContent.", len(response.Results), len(response.FailedResults))
	if response.Meta.RequestID != "" {
		fmt.Fprintf(&builder, "\nRequest ID: %s", boundedText(response.Meta.RequestID, 256))
	}
	for index, result := range response.Results {
		fmt.Fprintf(&builder, "\n\n%d. %s", index+1, boundedText(result.URL, 768))
		if result.Title != "" {
			fmt.Fprintf(&builder, "\nTitle: %s", boundedText(result.Title, 256))
		}
		if result.Provider != "" {
			fmt.Fprintf(&builder, "\nProvider: %s", boundedText(result.Provider, 64))
		}
		if result.Content != "" {
			fmt.Fprintf(&builder, "\nPreview: %s", boundedText(result.Content, maxMCPExtractPreviewRunes))
		}
	}
	for _, failure := range response.FailedResults {
		fmt.Fprintf(&builder, "\n\nFailed: %s", boundedText(failure.URL, 768))
		if failure.Error != "" {
			fmt.Fprintf(&builder, "\nError: %s", boundedText(failure.Error, 256))
		}
	}
	return boundedText(builder.String(), maxMCPExtractTextRunes)
}

func boundedText(value string, maxRunes int) string {
	runes := []rune(value)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return value
	}
	if maxRunes == 1 {
		return "…"
	}
	return string(runes[:maxRunes-1]) + "…"
}

func (h *Handler) handleMCPExtractTool(r *http.Request, requestID json.RawMessage, arguments json.RawMessage) (interface{}, *mcpResponse) {
	var extractReq model.ExtractRequest
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &extractReq); err != nil {
			return nil, mcpInvalidParams(requestID, "invalid extract arguments")
		}
	}
	if err := validateExtractRequest(extractReq); err != nil {
		return nil, mcpInvalidParams(requestID, err.Error())
	}
	extractReq.ProvidersExplicit = hasJSONField(arguments, "providers")
	extractReq.ChunksPerSourceSet = hasJSONField(arguments, "chunks_per_source")
	extractReq.CompatFormat = model.CompatFormatNative
	if extractReq.Options == nil {
		extractReq.Options = map[string]interface{}{}
	}
	extractReq.Options["source"] = "mcp"

	if token, ok := APIToken(r.Context()); ok {
		if !apiTokenHasScope(token, "extract") {
			return nil, &mcpResponse{JSONRPC: "2.0", ID: requestID, Error: &mcpError{Code: -32003, Message: "api token does not include extract scope"}}
		}
		filtered, err := applyTokenExtractProviders(extractReq.Providers, token.AllowedProviders)
		if err != nil {
			return nil, &mcpResponse{JSONRPC: "2.0", ID: requestID, Error: &mcpError{Code: -32003, Message: err.Error()}}
		}
		extractReq.Providers = filtered
	}

	response, err := h.orchestrator.Extract(r.Context(), extractReq, mcpInvocationRequestID(r), APITokenID(r.Context()))
	if err != nil {
		if message, ok := extractValidationMessage(err); ok {
			return nil, mcpInvalidParams(requestID, message)
		}
		return mcpToolError(err.Error()), nil
	}
	return mcpExtractToolResult(response), nil
}

func mcpExtractToolResult(response model.ExtractResponse) map[string]interface{} {
	return map[string]interface{}{
		"content":           []mcpContent{{Type: "text", Text: mcpExtractText(response)}},
		"structuredContent": response,
		"isError":           len(response.Results) == 0,
	}
}

func mcpInvocationRequestID(r *http.Request) string {
	invocationID := newRequestID()
	if parentID := RequestID(r.Context()); parentID != "" {
		return parentID + "-" + invocationID
	}
	return invocationID
}

func (h *Handler) mcpRequestsRequireAuth(requests []mcpRequest) bool {
	for _, req := range requests {
		if mcpMethodRequiresAuth(req.Method) {
			return true
		}
	}
	return false
}

func mcpMethodRequiresAuth(method string) bool {
	switch method {
	case "", "initialize", "notifications/initialized", "ping", "tools/list", "resources/list", "resources/templates/list", "prompts/list":
		return false
	default:
		return true
	}
}

func (h *Handler) mcpAuthContext(r *http.Request) (context.Context, int, string, error) {
	settings, err := h.store.RuntimeSettings(r.Context())
	if err != nil {
		return r.Context(), http.StatusInternalServerError, err.Error(), err
	}
	if !settings.APIAuthRequired {
		return r.Context(), http.StatusOK, "", nil
	}
	token := bearerToken(r)
	if token == "" {
		return r.Context(), http.StatusUnauthorized, "api token required", fmt.Errorf("api token required")
	}
	adminKey, ok, err := h.store.FindAdminAPIKey(r.Context(), token)
	if err != nil {
		return r.Context(), http.StatusInternalServerError, err.Error(), err
	}
	if ok {
		return context.WithValue(r.Context(), adminActorKey, adminAPIKeyActor(adminKey)), http.StatusOK, "", nil
	}
	apiToken, err := h.store.FindAPIToken(r.Context(), token)
	if err != nil {
		return r.Context(), http.StatusUnauthorized, "invalid api token", err
	}
	if !h.auth.allowToken(apiToken) {
		return r.Context(), http.StatusTooManyRequests, "api token rate limit exceeded", fmt.Errorf("api token rate limit exceeded")
	}
	ctx := context.WithValue(r.Context(), apiTokenIDKey, apiToken.ID)
	ctx = context.WithValue(ctx, apiTokenKey, apiToken)
	return ctx, http.StatusOK, "", nil
}

func mcpInitializeResult(params json.RawMessage) map[string]interface{} {
	return map[string]interface{}{
		"protocolVersion": negotiateMCPProtocolVersion(params),
		"capabilities": map[string]interface{}{
			"tools":     map[string]interface{}{"listChanged": false},
			"resources": map[string]interface{}{"listChanged": false},
			"prompts":   map[string]interface{}{"listChanged": false},
		},
		"serverInfo": map[string]interface{}{
			"name":    "one-search-relay",
			"title":   "One Search Relay",
			"version": "0.1.0",
		},
		"instructions": "Use tools/call with search for web search or extract to fetch content from known URLs through configured One Search Relay providers.",
	}
}

func negotiateMCPProtocolVersion(params json.RawMessage) string {
	if len(params) == 0 {
		return mcpDefaultProtocolVersion
	}
	var payload struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return mcpDefaultProtocolVersion
	}
	for _, version := range mcpSupportedProtocolVersions {
		if payload.ProtocolVersion == version {
			return version
		}
	}
	return mcpDefaultProtocolVersion
}

func mcpSearchToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"name":        "search",
		"title":       "One Search",
		"description": "Search the web through configured One Search Relay providers.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Search query.",
				},
				"providers": map[string]interface{}{
					"type":        "array",
					"description": "Optional providers to use. Defaults to runtime settings.",
					"items":       map[string]interface{}{"type": "string", "enum": model.DefaultProviders},
				},
				"mode": map[string]interface{}{
					"type":        "string",
					"description": "Search mode.",
					"enum":        []string{string(model.SearchModeParallel), string(model.SearchModeFallback), string(model.SearchModeSingle)},
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum results, capped at 50.",
					"minimum":     1,
					"maximum":     50,
				},
				"freshness": map[string]interface{}{
					"type":        "string",
					"description": "Optional freshness hint.",
				},
				"dedupe": map[string]interface{}{
					"type":        "boolean",
					"description": "Whether to deduplicate results by URL.",
				},
				"cache": map[string]interface{}{
					"type":        "string",
					"description": "Cache policy.",
					"enum":        []string{string(model.CachePolicyDefault), string(model.CachePolicyBypass), string(model.CachePolicyRefresh)},
				},
				"include_raw": map[string]interface{}{
					"type":        "boolean",
					"description": "Whether to include raw upstream result items.",
				},
			},
			"required": []string{"query"},
		},
		"annotations": map[string]interface{}{
			"title":         "One Search",
			"readOnlyHint":  true,
			"openWorldHint": true,
		},
	}
}

func mcpExtractToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"name":        "extract",
		"title":       "One Search Extract",
		"description": "Extract page content from known URLs through configured One Search Relay providers.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"dependentRequired": map[string]interface{}{
				"chunks_per_source": []string{"query"},
			},
			"properties": map[string]interface{}{
				"urls": map[string]interface{}{
					"type":        "array",
					"description": "URLs to extract, up to 20 per call.",
					"items":       map[string]interface{}{"type": "string", "format": "uri"},
					"minItems":    1,
					"maxItems":    20,
				},
				"providers": map[string]interface{}{
					"type":        "array",
					"description": "Optional extract-capable providers to use. Defaults to runtime settings.",
					"items":       map[string]interface{}{"type": "string", "enum": model.ExtractProviders},
				},
				"mode": map[string]interface{}{
					"type":        "string",
					"description": "Provider execution mode.",
					"enum":        []string{string(model.SearchModeParallel), string(model.SearchModeFallback), string(model.SearchModeSingle)},
				},
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Optional query used to focus extracted content when supported.",
					"minLength":   1,
				},
				"format": map[string]interface{}{
					"type":        "string",
					"description": "Requested output format.",
					"enum":        []string{string(model.ExtractFormatMarkdown), string(model.ExtractFormatText), string(model.ExtractFormatHTML), string(model.ExtractFormatRawHTML)},
				},
				"extract_depth": map[string]interface{}{
					"type":        "string",
					"description": "Extraction depth hint.",
					"enum":        []string{"basic", "advanced"},
				},
				"chunks_per_source": map[string]interface{}{
					"type":        "integer",
					"description": "Number of relevant chunks per source when query-focused extraction is supported.",
					"minimum":     1,
					"maximum":     5,
				},
				"include_images": map[string]interface{}{
					"type":        "boolean",
					"description": "Whether to include extracted image URLs when supported.",
				},
				"include_favicon": map[string]interface{}{
					"type":        "boolean",
					"description": "Whether to include a favicon URL when supported.",
				},
				"include_raw": map[string]interface{}{
					"type":        "boolean",
					"description": "Whether to include raw upstream payload fields.",
				},
			},
			"required": []string{"urls"},
		},
		"annotations": map[string]interface{}{
			"title":         "One Search Extract",
			"readOnlyHint":  true,
			"openWorldHint": true,
		},
	}
}

func mcpToolError(message string) map[string]interface{} {
	return map[string]interface{}{
		"content": []mcpContent{{Type: "text", Text: message}},
		"isError": true,
	}
}

func mcpInvalidParams(id json.RawMessage, message string) *mcpResponse {
	response := newMCPError(id, -32602, message, nil)
	return &response
}

func newMCPResult(id json.RawMessage, result interface{}) mcpResponse {
	return mcpResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func newMCPError(id json.RawMessage, code int, message string, data interface{}) mcpResponse {
	if id == nil {
		id = json.RawMessage("null")
	}
	return mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: code, Message: message, Data: data}}
}

func writeMCPError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string, data interface{}) {
	writeMCPResponse(w, status, newMCPError(id, code, message, data))
}

func writeMCPAccepted(w http.ResponseWriter) {
	w.Header().Set("Mcp-Protocol-Version", mcpLatestProtocolVersion)
	w.WriteHeader(http.StatusAccepted)
}

func writeMCPResponse(w http.ResponseWriter, status int, response mcpResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Mcp-Protocol-Version", mcpLatestProtocolVersion)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func writeMCPBatchResponse(w http.ResponseWriter, status int, responses []mcpResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Mcp-Protocol-Version", mcpLatestProtocolVersion)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(responses)
}

func firstMCPRequestID(body []byte) json.RawMessage {
	if len(body) == 0 {
		return nil
	}
	if body[0] == '[' {
		var requests []mcpRequest
		if err := json.Unmarshal(body, &requests); err == nil && len(requests) > 0 {
			return requests[0].ID
		}
		return nil
	}
	var req mcpRequest
	if err := json.Unmarshal(body, &req); err == nil {
		return req.ID
	}
	return nil
}
