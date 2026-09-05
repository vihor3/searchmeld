package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vihor3/searchmeld/backend/internal/model"
)

func TestRequireAPITokenScope(t *testing.T) {
	tests := []struct {
		name       string
		settings   model.RuntimeSettings
		token      model.APIToken
		admin      bool
		authorize  bool
		wantStatus int
	}{
		{name: "matching scope", settings: model.RuntimeSettings{APIAuthRequired: true}, token: model.APIToken{ID: 1, Scopes: []string{"extract"}}, authorize: true, wantStatus: http.StatusNoContent},
		{name: "missing scope", settings: model.RuntimeSettings{APIAuthRequired: true}, token: model.APIToken{ID: 1, Scopes: []string{"search"}}, authorize: true, wantStatus: http.StatusForbidden},
		{name: "admin key bypass", settings: model.RuntimeSettings{APIAuthRequired: true}, admin: true, authorize: true, wantStatus: http.StatusNoContent},
		{name: "auth disabled", settings: model.RuntimeSettings{APIAuthRequired: false}, wantStatus: http.StatusNoContent},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &scopeAuthStore{settings: test.settings, token: test.token, admin: test.admin}
			auth := NewAuthService(store, 0, 0, 0, 0)
			handler := auth.requireAPITokenScope("extract")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			request := httptest.NewRequest(http.MethodPost, "/v1/extract", nil)
			if test.authorize {
				request.Header.Set("Authorization", "Bearer test-token")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

func TestRequireTavilyAPITokenScopeAcceptsLegacyBodyAPIKey(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		headerToken   string
		tokenScopes   []string
		expectedToken string
		wantStatus    int
	}{
		{
			name:          "body api key",
			body:          `{"query":"one search","api_key":"osr-body"}`,
			tokenScopes:   []string{"search"},
			expectedToken: "osr-body",
			wantStatus:    http.StatusNoContent,
		},
		{
			name:          "header takes precedence",
			body:          `{"query":"one search","api_key":"osr-body"}`,
			headerToken:   "osr-header",
			tokenScopes:   []string{"search"},
			expectedToken: "osr-header",
			wantStatus:    http.StatusNoContent,
		},
		{
			name:          "body api key still enforces scope",
			body:          `{"query":"one search","api_key":"osr-body"}`,
			tokenScopes:   []string{"extract"},
			expectedToken: "osr-body",
			wantStatus:    http.StatusForbidden,
		},
		{
			name:        "missing api key",
			body:        `{"query":"one search"}`,
			tokenScopes: []string{"search"},
			wantStatus:  http.StatusUnauthorized,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &scopeAuthStore{
				settings:      model.RuntimeSettings{APIAuthRequired: true},
				token:         model.APIToken{ID: 1, Scopes: test.tokenScopes},
				expectedToken: test.expectedToken,
			}
			auth := NewAuthService(store, 0, 0, 0, 0)
			var receivedBody string
			handler := auth.requireTavilyAPITokenScope("search")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read restored body: %v", err)
				}
				receivedBody = string(body)
				w.WriteHeader(http.StatusNoContent)
			}))
			request := httptest.NewRequest(http.MethodPost, "/v1/compat/tavily/search", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			if test.headerToken != "" {
				request.Header.Set("Authorization", "Bearer "+test.headerToken)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			if test.expectedToken != "" && store.seenToken != test.expectedToken {
				t.Fatalf("authenticated token = %q, want %q", store.seenToken, test.expectedToken)
			}
			if test.wantStatus == http.StatusNoContent && receivedBody != test.body {
				t.Fatalf("restored body = %q, want %q", receivedBody, test.body)
			}
		})
	}
}

func TestLegacyTavilyBodyAPIKeyIsNotAcceptedByNativeRoutes(t *testing.T) {
	store := &scopeAuthStore{
		settings: model.RuntimeSettings{APIAuthRequired: true},
		token:    model.APIToken{ID: 1, Scopes: []string{"search"}},
	}
	auth := NewAuthService(store, 0, 0, 0, 0)
	handler := auth.requireAPITokenScope("search")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"query":"one search","api_key":"osr-body"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusUnauthorized, response.Body.String())
	}
}

func TestRequireAPITokenCountsAllUnmarkedResponses(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		wantUsageMarks int
	}{
		{name: "accepted", status: http.StatusNoContent, wantUsageMarks: 1},
		{name: "success", status: http.StatusOK, wantUsageMarks: 1},
		{name: "bad request", status: http.StatusBadRequest, wantUsageMarks: 1},
		{name: "unrelated forbidden", status: http.StatusForbidden, wantUsageMarks: 1},
		{name: "provider rate limited", status: http.StatusTooManyRequests, wantUsageMarks: 1},
		{name: "internal error", status: http.StatusInternalServerError, wantUsageMarks: 1},
		{name: "provider error", status: http.StatusBadGateway, wantUsageMarks: 1},
		{name: "no provider key", status: http.StatusServiceUnavailable, wantUsageMarks: 1},
		{name: "provider timeout", status: http.StatusGatewayTimeout, wantUsageMarks: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &scopeAuthStore{
				settings:      model.RuntimeSettings{APIAuthRequired: true},
				token:         model.APIToken{ID: 1, Scopes: []string{"search", "extract"}},
				expectedToken: "test-token",
			}
			auth := NewAuthService(store, 0, 0, 0, 0)
			handler := auth.requireAPIToken(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
			}))
			request := httptest.NewRequest(http.MethodPost, "/v1/test", nil)
			request.Header.Set("Authorization", "Bearer test-token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			if store.usageMarks != test.wantUsageMarks {
				t.Fatalf("usage marks = %d, want %d", store.usageMarks, test.wantUsageMarks)
			}
		})
	}
}

func TestMountedSearchRejectionsStillCount(t *testing.T) {
	for _, test := range []struct {
		name   string
		path   string
		scopes []string
	}{
		{name: "native scope", path: "/v1/search", scopes: []string{"extract"}},
		{name: "tavily scope", path: "/v1/compat/tavily/search", scopes: []string{"extract"}},
		{name: "native provider", path: "/v1/search", scopes: []string{"search"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &rejectedRequestLogRecorder{scopeAuthStore: &scopeAuthStore{
				settings: model.RuntimeSettings{APIAuthRequired: true},
				token:    model.APIToken{ID: 1, Scopes: test.scopes, AllowedProviders: []string{model.ProviderBrave}},
			}}
			h := NewHandler(store, NewAuthService(store, 0, 0, 0, 0), nil)
			router := chi.NewRouter()
			h.Mount(router)
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(`{"query":"test","providers":["tavily"]}`))
			request.Header.Set("Authorization", "Bearer test-token")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || store.usageMarks != 1 || len(store.inputs) != 0 {
				t.Fatalf("search rejection: status=%d usage marks=%d rejected logs=%d", response.Code, store.usageMarks, len(store.inputs))
			}
		})
	}
}

func TestMountedExtractRateLimitStillCounts(t *testing.T) {
	for _, path := range []string{"/v1/extract", "/v1/compat/tavily/extract"} {
		t.Run(path, func(t *testing.T) {
			store := &rejectedRequestLogRecorder{scopeAuthStore: &scopeAuthStore{
				settings: model.RuntimeSettings{APIAuthRequired: true},
				token:    model.APIToken{ID: 1, Scopes: []string{"search"}, RateLimitPerMin: 1},
			}}
			auth := NewAuthService(store, 0, 0, 0, 0)
			auth.rateWindows[1] = rateWindow{StartedAt: time.Now(), Count: 1}
			h := NewHandler(store, auth, nil)
			router := chi.NewRouter()
			h.Mount(router)
			request := httptest.NewRequest(http.MethodPost, path, nil)
			request.Header.Set("Authorization", "Bearer test-token")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusTooManyRequests || store.usageMarks != 1 || len(store.inputs) != 0 {
				t.Fatalf("rate limit: status=%d usage marks=%d rejected logs=%d", response.Code, store.usageMarks, len(store.inputs))
			}
		})
	}
}

func TestRequireAPITokenCountsHandlerPanic(t *testing.T) {
	store := &scopeAuthStore{
		settings: model.RuntimeSettings{APIAuthRequired: true},
		token:    model.APIToken{ID: 1},
	}
	handler := NewAuthService(store, 0, 0, 0, 0).requireAPIToken(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("test handler panic")
	}))
	request := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	defer func() {
		if recovered := recover(); recovered != "test handler panic" {
			t.Fatalf("panic = %v, want test handler panic", recovered)
		}
		if store.usageMarks != 1 {
			t.Fatalf("usage marks = %d, want 1", store.usageMarks)
		}
	}()
	handler.ServeHTTP(httptest.NewRecorder(), request)
}

func TestMountedMCPCountingUnchanged(t *testing.T) {
	for _, test := range []struct {
		name        string
		body        string
		scopes      []string
		rateLimited bool
		wantStatus  int
		wantMarks   int
		wantError   int
	}{
		{name: "discovery", body: `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, wantStatus: http.StatusOK},
		{name: "scope rejection", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"extract","arguments":{"urls":["https://example.com"]}}}`, scopes: []string{"search"}, wantStatus: http.StatusOK, wantMarks: 1, wantError: -32003},
		{name: "provider rejection", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"extract","arguments":{"urls":["https://example.com"],"providers":["tavily"]}}}`, scopes: []string{"extract"}, wantStatus: http.StatusOK, wantMarks: 1, wantError: -32003},
		{name: "rate limited", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"extract","arguments":{"urls":["https://example.com"]}}}`, rateLimited: true, wantStatus: http.StatusTooManyRequests, wantMarks: 1, wantError: -32001},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &rejectedRequestLogRecorder{scopeAuthStore: &scopeAuthStore{
				settings: model.RuntimeSettings{APIAuthRequired: true},
				token:    model.APIToken{ID: 1, Scopes: test.scopes, AllowedProviders: []string{model.ProviderBrave}, RateLimitPerMin: 1},
			}}
			auth := NewAuthService(store, 0, 0, 0, 0)
			if test.rateLimited {
				auth.rateWindows[1] = rateWindow{StartedAt: time.Now(), Count: 1}
			}
			h := NewHandler(store, auth, nil)
			h.EnableMCP("/mcp")
			router := chi.NewRouter()
			h.Mount(router)
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer test-token")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.wantStatus || store.usageMarks != test.wantMarks || len(store.inputs) != 0 {
				t.Fatalf("MCP request: status=%d usage marks=%d rejected logs=%d", response.Code, store.usageMarks, len(store.inputs))
			}
			var result mcpResponse
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatalf("decode MCP response: %v", err)
			}
			if test.wantError == 0 {
				if result.Error != nil {
					t.Fatalf("unexpected MCP error: %+v", result.Error)
				}
			} else if result.Error == nil || result.Error.Code != test.wantError {
				t.Fatalf("MCP error = %+v, want code %d", result.Error, test.wantError)
			}
		})
	}
}

type scopeAuthStore struct {
	AppStore
	settings      model.RuntimeSettings
	token         model.APIToken
	admin         bool
	expectedToken string
	seenToken     string
	usageMarks    int
}

func (s *scopeAuthStore) GetAdminByUsername(context.Context, string) (model.AdminUser, error) {
	return model.AdminUser{}, nil
}

func (s *scopeAuthStore) FindAdminAPIKey(context.Context, string) (model.AdminAPIKey, bool, error) {
	return model.AdminAPIKey{}, s.admin, nil
}

func (s *scopeAuthStore) FindAPIToken(_ context.Context, token string) (model.APIToken, error) {
	s.seenToken = token
	if s.expectedToken != "" && token != s.expectedToken {
		return model.APIToken{}, errors.New("unexpected api token")
	}
	return s.token, nil
}

func (s *scopeAuthStore) MarkAPITokenUsed(context.Context, int64) error {
	s.usageMarks++
	return nil
}

func (s *scopeAuthStore) RuntimeSettings(context.Context) (model.RuntimeSettings, error) {
	return s.settings, nil
}

func (*scopeAuthStore) ListProviders(context.Context) ([]model.ProviderConfig, error) {
	return []model.ProviderConfig{}, nil
}
