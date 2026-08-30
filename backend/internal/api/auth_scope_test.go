package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/one-search/one-search/backend/internal/model"
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

type scopeAuthStore struct {
	settings      model.RuntimeSettings
	token         model.APIToken
	admin         bool
	expectedToken string
	seenToken     string
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

func (s *scopeAuthStore) RuntimeSettings(context.Context) (model.RuntimeSettings, error) {
	return s.settings, nil
}
