package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDMiddlewareMakesClientIDsUnique(t *testing.T) {
	handler := requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Context-Request-ID", RequestID(r.Context()))
		w.WriteHeader(http.StatusNoContent)
	}))

	requestID := func() string {
		request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		request.Header.Set("X-Request-ID", " client-trace-42 ")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		got := response.Header().Get("X-Request-ID")
		if got == "" || got != response.Header().Get("X-Context-Request-ID") {
			t.Fatalf("request IDs do not match: response=%q context=%q", got, response.Header().Get("X-Context-Request-ID"))
		}
		if !strings.HasPrefix(got, "client-trace-42-") {
			t.Fatalf("request ID %q does not preserve the client correlation prefix", got)
		}
		return got
	}

	first := requestID()
	second := requestID()
	if first == second {
		t.Fatalf("reused client X-Request-ID produced duplicate internal ID %q", first)
	}
}

func TestClientRequestIDPrefixIsBoundedAndSanitized(t *testing.T) {
	got := clientRequestIDPrefix("../unsafe request id/" + strings.Repeat("x", 100))
	if len(got) > 32 || strings.ContainsAny(got, "/ ") {
		t.Fatalf("unsafe request ID prefix = %q", got)
	}
}

func TestMCPInvocationRequestIDsAreUniqueWithinOneHTTPRequest(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(context.WithValue(request.Context(), requestIDKey, "parent-request"))
	first := mcpInvocationRequestID(request)
	second := mcpInvocationRequestID(request)
	if first == second || !strings.HasPrefix(first, "parent-request-") || !strings.HasPrefix(second, "parent-request-") {
		t.Fatalf("MCP invocation IDs are not unique children: first=%q second=%q", first, second)
	}
}
