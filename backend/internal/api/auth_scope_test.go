package api

import (
	"context"
	"net/http"
	"net/http/httptest"
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

type scopeAuthStore struct {
	settings model.RuntimeSettings
	token    model.APIToken
	admin    bool
}

func (s *scopeAuthStore) GetAdminByUsername(context.Context, string) (model.AdminUser, error) {
	return model.AdminUser{}, nil
}

func (s *scopeAuthStore) FindAdminAPIKey(context.Context, string) (model.AdminAPIKey, bool, error) {
	return model.AdminAPIKey{}, s.admin, nil
}

func (s *scopeAuthStore) FindAPIToken(context.Context, string) (model.APIToken, error) {
	return s.token, nil
}

func (s *scopeAuthStore) RuntimeSettings(context.Context) (model.RuntimeSettings, error) {
	return s.settings, nil
}
