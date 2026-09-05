package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/vihor3/searchmeld/backend/internal/model"
)

func TestMountedExtractRejections(t *testing.T) {
	for _, transport := range []struct {
		path         string
		compatFormat model.CompatFormat
	}{
		{path: "/v1/extract", compatFormat: model.CompatFormatNative},
		{path: "/v1/compat/tavily/extract", compatFormat: model.CompatFormatTavily},
	} {
		for _, test := range []struct {
			name       string
			scope      string
			body       string
			message    string
			wantBody   string
			tavilyBody string
		}{
			{
				name:     "scope",
				scope:    "search",
				body:     `{"urls":["https://example.com/private?sig=url-secret"],"api_key":"extract-test-secret"}`,
				message:  "api token does not include extract scope",
				wantBody: "{\"error\":{\"message\":\"api token does not include extract scope\",\"status\":403}}\n",
			},
			{
				name:       "provider",
				scope:      "extract",
				body:       `{"urls":["https://user:password@example.com/private?api_key=url-secret#fragment-secret"],"providers":["tavily"],"query":"query-secret","options":{"api_key":"option-secret"},"api_key":"extract-test-secret","timeout":1}`,
				message:    "api token provider allowlist rejected extract request",
				wantBody:   "{\"error\":{\"message\":\"api token is not allowed to request provider tavily\",\"status\":403}}\n",
				tavilyBody: "{\"detail\":{\"error\":\"api token is not allowed to request provider tavily\"}}\n",
			},
			{
				name:       "unknown provider",
				scope:      "extract",
				body:       `{"urls":["https://example.com"],"providers":["api_key=provider-secret"],"api_key":"extract-test-secret"}`,
				message:    "api token provider allowlist rejected extract request",
				wantBody:   "{\"error\":{\"message\":\"api token is not allowed to request provider api_key=provider-secret\",\"status\":403}}\n",
				tavilyBody: "{\"detail\":{\"error\":\"api token is not allowed to request provider api_key=provider-secret\"}}\n",
			},
			{
				name:       "no extract-capable allowed provider",
				scope:      "extract",
				body:       `{"urls":["https://example.com"],"api_key":"extract-test-secret"}`,
				message:    "api token provider allowlist rejected extract request",
				wantBody:   "{\"error\":{\"message\":\"api token is not allowed to use an extract-capable provider\",\"status\":403}}\n",
				tavilyBody: "{\"detail\":{\"error\":\"api token is not allowed to use an extract-capable provider\"}}\n",
			},
		} {
			for _, persistence := range []struct {
				name string
				err  error
			}{
				{name: "logged"},
				{name: "log write fails", err: errors.New("database unavailable")},
			} {
				t.Run(string(transport.compatFormat)+"/"+test.name+"/"+persistence.name, func(t *testing.T) {
					store := &rejectedRequestLogRecorder{
						scopeAuthStore: &scopeAuthStore{
							settings: model.RuntimeSettings{APIAuthRequired: true, CompatTavilyEnabled: true},
							token: model.APIToken{
								ID: 42, Scopes: []string{test.scope}, AllowedProviders: []string{model.ProviderBrave},
							},
							expectedToken: "extract-test-secret",
						},
						err: persistence.err,
					}
					logger := &rejectedLogTestLogger{}
					h := NewHandler(store, NewAuthService(store, 0, 0, 0, 0), nil)
					h.SetLogger(logger)
					router := chi.NewRouter()
					router.Use(requestIDMiddleware)
					h.Mount(router)

					body := strings.NewReader(test.body)
					request := httptest.NewRequest(http.MethodPost, transport.path, body)
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("X-Request-ID", "client-correlation")
					if transport.compatFormat == model.CompatFormatNative {
						request.Header.Set("Authorization", "Bearer extract-test-secret")
					}
					response := httptest.NewRecorder()
					router.ServeHTTP(response, request)

					wantBody := test.wantBody
					if transport.compatFormat == model.CompatFormatTavily && test.tavilyBody != "" {
						wantBody = test.tavilyBody
					}
					if response.Code != http.StatusForbidden || response.Body.String() != wantBody {
						t.Fatalf("response = %d %q, want 403 %q", response.Code, response.Body.String(), wantBody)
					}
					if test.scope == "search" && transport.compatFormat == model.CompatFormatNative && body.Len() != len(test.body) {
						t.Fatal("scope rejection read the request body")
					}
					if store.usageMarks != 0 {
						t.Fatalf("rejection recorded %d token usages", store.usageMarks)
					}
					if len(store.inputs) != 1 {
						t.Fatalf("rejected logs = %d, want 1", len(store.inputs))
					}
					input := store.inputs[0]
					if input.RequestID == "" || input.RequestID == "client-correlation" || input.RequestID != response.Header().Get("X-Request-ID") {
						t.Fatalf("request id = %q, response header = %q", input.RequestID, response.Header().Get("X-Request-ID"))
					}
					if input.APITokenID != 42 || input.Operation != "extract" || input.CompatFormat != string(transport.compatFormat) {
						t.Fatalf("unexpected rejected log identity: %+v", input)
					}
					if input.Status != "error" || input.CachePolicy != string(model.CachePolicyBypass) || input.ErrorMessage != test.message || input.LatencyMS < 0 {
						t.Fatalf("unexpected rejected log status: %+v", input)
					}
					if string(input.RequestJSON) != "{}" || string(input.ResponseJSON) != "{}" || input.Query != "" || input.Mode != "" || len(input.Providers) != 0 || len(input.Calls) != 0 || input.CacheHit || input.ResultCount != 0 {
						t.Fatalf("rejection retained request metadata or accounting: %+v", input)
					}
					serialized, err := json.Marshal(input)
					if err != nil {
						t.Fatalf("marshal rejected log: %v", err)
					}
					for _, secret := range []string{"extract-test-secret", "example.com", "user", "password", "api_key", "url-secret", "fragment-secret", "provider-secret", "query-secret", "option-secret"} {
						if strings.Contains(string(serialized), secret) {
							t.Fatalf("rejected log contains sensitive value %q", secret)
						}
					}
					if persistence.err == nil {
						if len(logger.errors) != 0 {
							t.Fatalf("unexpected structured errors: %+v", logger.errors)
						}
					} else {
						if len(logger.errors) != 1 || logger.errors[0].message != "rejected_request_log_failed" {
							t.Fatalf("unexpected structured errors: %+v", logger.errors)
						}
						fields := logger.errors[0].fields
						if fields["request_id"] != input.RequestID || fields["operation"] != "extract" || fields["compat_format"] != string(transport.compatFormat) || fields["error"] != "database unavailable" {
							t.Fatalf("unexpected structured error fields: %+v", fields)
						}
					}

					accepted := httptest.NewRequest(http.MethodGet, "/v1/providers", nil)
					accepted.Header.Set("Authorization", "Bearer extract-test-secret")
					response = httptest.NewRecorder()
					router.ServeHTTP(response, accepted)
					if response.Code != http.StatusOK || store.usageMarks != 1 || len(store.inputs) != 1 {
						t.Fatalf("later accepted request: status=%d usage marks=%d rejected logs=%d", response.Code, store.usageMarks, len(store.inputs))
					}
				})
			}
		}
	}
}

func TestMountedExtractAuthBoundaries(t *testing.T) {
	for _, path := range []string{"/v1/extract", "/v1/compat/tavily/extract"} {
		for _, test := range []struct {
			name         string
			credential   string
			scopes       []string
			admin        bool
			authDisabled bool
			wantStatus   int
			wantMarks    int
		}{
			{name: "missing credential", wantStatus: http.StatusUnauthorized},
			{name: "invalid credential", credential: "invalid-token", wantStatus: http.StatusUnauthorized},
			{name: "admin key bypass", credential: "valid-token", admin: true, wantStatus: http.StatusBadRequest},
			{name: "auth disabled", authDisabled: true, wantStatus: http.StatusBadRequest},
			{name: "matching scope", credential: "valid-token", scopes: []string{"extract"}, wantStatus: http.StatusBadRequest, wantMarks: 1},
			{name: "case insensitive scope", credential: "valid-token", scopes: []string{" Extract "}, wantStatus: http.StatusBadRequest, wantMarks: 1},
			{name: "wildcard scope", credential: "valid-token", scopes: []string{"*"}, wantStatus: http.StatusBadRequest, wantMarks: 1},
		} {
			t.Run(path+"/"+test.name, func(t *testing.T) {
				store := &rejectedRequestLogRecorder{scopeAuthStore: &scopeAuthStore{
					settings: model.RuntimeSettings{APIAuthRequired: !test.authDisabled, CompatTavilyEnabled: true},
					token: model.APIToken{
						ID: 42, Scopes: test.scopes, AllowedProviders: []string{model.ProviderBrave},
					},
					admin: test.admin, expectedToken: "valid-token",
				}}
				h := NewHandler(store, NewAuthService(store, 0, 0, 0, 0), nil)
				router := chi.NewRouter()
				h.Mount(router)
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"api_key":"`+test.credential+`"}`))
				if path == "/v1/extract" && test.credential != "" {
					request.Header.Set("Authorization", "Bearer "+test.credential)
				}
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != test.wantStatus {
					t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body.String())
				}
				if test.wantStatus == http.StatusBadRequest && !strings.Contains(response.Body.String(), "urls are required") {
					t.Fatalf("request did not reach extract validation: %s", response.Body.String())
				}
				if len(store.inputs) != 0 || store.usageMarks != test.wantMarks {
					t.Fatalf("rejected logs=%d usage marks=%d, want 0 and %d", len(store.inputs), store.usageMarks, test.wantMarks)
				}
			})
		}
	}
}

func TestExtractRejectionWithoutStoreDoesNotCount(t *testing.T) {
	store := &scopeAuthStore{
		settings: model.RuntimeSettings{APIAuthRequired: true},
		token:    model.APIToken{ID: 42, Scopes: []string{"search"}},
	}
	h := NewHandler(nil, NewAuthService(store, 0, 0, 0, 0), nil)
	router := chi.NewRouter()
	h.Mount(router)
	for _, path := range []string{"/v1/extract", "/v1/compat/tavily/extract"} {
		body := strings.NewReader("not valid json")
		request := httptest.NewRequest(http.MethodPost, path, body)
		request.Header.Set("Authorization", "Bearer test-token")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || store.usageMarks != 0 || body.Len() != len("not valid json") {
			t.Fatalf("rejection without store: status=%d usage marks=%d unread body=%d", response.Code, store.usageMarks, body.Len())
		}
	}
}

type rejectedRequestLogRecorder struct {
	*scopeAuthStore
	inputs []model.SearchLogInput
	err    error
}

func (r *rejectedRequestLogRecorder) RecordRejectedRequestLog(_ context.Context, input model.SearchLogInput) error {
	r.inputs = append(r.inputs, input)
	return r.err
}

type rejectedLogEvent struct {
	message string
	fields  map[string]interface{}
}

type rejectedLogTestLogger struct {
	errors []rejectedLogEvent
}

func (*rejectedLogTestLogger) Info(string, map[string]interface{}) {}

func (l *rejectedLogTestLogger) Error(message string, fields map[string]interface{}) {
	l.errors = append(l.errors, rejectedLogEvent{message: message, fields: fields})
}
