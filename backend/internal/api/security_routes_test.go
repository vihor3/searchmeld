package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vihor3/searchmeld/backend/internal/config"
	"github.com/vihor3/searchmeld/backend/internal/model"
	"github.com/vihor3/searchmeld/backend/internal/provider"
	"github.com/vihor3/searchmeld/backend/internal/search"
)

// TestMountedSearchProviderPolicy exercises all HTTP search transports with Token
// restrictions and credential bypasses, preserving admission, usage and envelopes
// while rejecting excluded providers before key, cache or orchestration work.
func TestMountedSearchProviderPolicy(t *testing.T) {
	for _, route := range []struct {
		path           string
		queryField     string
		limitField     string
		resultField    string
		requestIDField string
		responseFields []string
		format         model.CompatFormat
	}{
		{
			path: "/v1/search", queryField: "query", limitField: "limit", resultField: "results", format: model.CompatFormatNative,
			responseFields: []string{"results", "providers", "meta"},
		},
		{
			path: "/v1/compat/tavily/search", queryField: "query", limitField: "max_results", resultField: "results", format: model.CompatFormatTavily,
			requestIDField: "request_id", responseFields: []string{"query", "results", "response_time", "request_id"},
		},
		{
			path: "/v1/compat/serper/search", queryField: "q", limitField: "num", resultField: "organic", format: model.CompatFormatSerper,
			requestIDField: "requestId", responseFields: []string{"searchParameters", "organic", "credits", "requestId"},
		},
		{
			path: "/v1/compat/openai/responses-search", queryField: "input", limitField: "limit", resultField: "search_results", format: model.CompatFormatOpenAI,
			requestIDField: "id", responseFields: []string{"id", "object", "status", "search_results", "output"},
		},
	} {
		for _, identity := range []struct {
			name         string
			credential   string
			authRequired bool
			allowed      []string
			restricted   bool
			wantTokenID  int64
		}{
			{name: "restrictive Token", credential: "osr_provider-policy", authRequired: true, allowed: []string{model.ProviderBrave}, restricted: true, wantTokenID: 42},
			{name: "unrestricted Token", credential: "osr_provider-policy", authRequired: true, wantTokenID: 42},
			{name: "empty allowlist Token", credential: "osr_provider-policy", authRequired: true, allowed: []string{}, wantTokenID: 42},
			{name: "admin Key", credential: adminTestKey, authRequired: true, allowed: []string{model.ProviderBrave}},
			{name: "auth off anonymous", allowed: []string{model.ProviderBrave}},
			{name: "auth off Token", credential: "osr_provider-policy", allowed: []string{model.ProviderBrave}},
			{name: "auth off admin Key", credential: adminTestKey, allowed: []string{model.ProviderBrave}},
		} {
			for _, test := range []struct {
				name          string
				providers     []string
				wantProviders []string
				denied        bool
			}{
				{name: "excluded global default", wantProviders: []string{model.ProviderSerper}},
				{name: "empty explicit providers", providers: []string{}, wantProviders: []string{model.ProviderSerper}},
				{name: "allowed explicit", providers: []string{model.ProviderBrave}, wantProviders: []string{model.ProviderBrave}},
				{name: "excluded explicit", providers: []string{model.ProviderSerper}, wantProviders: []string{model.ProviderSerper}, denied: true},
				{name: "mixed explicit", providers: []string{model.ProviderBrave, model.ProviderSerper}, wantProviders: []string{model.ProviderBrave, model.ProviderSerper}, denied: true},
			} {
				t.Run(route.path+"/"+identity.name+"/"+test.name, func(t *testing.T) {
					brave := &policyTestProvider{name: model.ProviderBrave}
					serper := &policyTestProvider{name: model.ProviderSerper}
					pool := &policyTestKeyPool{}
					store := &policyTestStore{
						scopeAuthStore: &scopeAuthStore{
							settings: model.RuntimeSettings{
								APIAuthRequired: identity.authRequired, CompatTavilyEnabled: true, CompatSerperEnabled: true, CompatOpenAIEnabled: true,
								DefaultProviders: []string{model.ProviderSerper}, DefaultLimit: 11, CacheEnabled: true,
							},
							token:         model.APIToken{ID: 42, Scopes: []string{"search"}, AllowedProviders: identity.allowed, RateLimitPerMin: 10},
							expectedToken: "osr_provider-policy",
						},
					}
					orchestrator := search.NewOrchestrator(provider.NewRegistry(brave, serper), pool, store)
					auth := NewAuthService(store, 0, 0, 0, 0)
					h := NewHandler(store, auth, orchestrator)
					server := NewServer(config.Config{RequestBodyLimitBytes: 1024 * 1024}, &adminAuthTestLogger{})
					server.Mount(h.Mount)
					body := map[string]interface{}{route.queryField: "provider policy", "mode": "parallel"}
					wantLimit := 11
					if test.providers != nil {
						body["providers"] = test.providers
						body[route.limitField] = 7
						wantLimit = 7
					}
					encoded, err := json.Marshal(body)
					if err != nil {
						t.Fatalf("encode request: %v", err)
					}
					r := adminTestRequest(http.MethodPost, route.path, string(encoded), nil)
					r.Header.Set("X-Request-ID", "policy-correlation")
					if identity.credential != "" {
						r.Header.Set("Authorization", "Bearer "+identity.credential)
					}
					denied := identity.restricted && test.denied
					wantStatus := http.StatusOK
					if denied {
						wantStatus = http.StatusForbidden
					}
					response := adminTestResponse(t, server.Router(), r, wantStatus)
					requestID := response.Header().Get("X-Request-ID")
					if requestID == "" || requestID == "policy-correlation" {
						t.Fatalf("server request identity missing: %q", requestID)
					}
					wantMarks := 0
					if identity.wantTokenID != 0 {
						wantMarks = 1
					}
					if store.usageMarks != wantMarks || auth.rateWindows[42].Count != wantMarks {
						t.Fatalf("Token admission: marks=%d rate count=%d, want %d", store.usageMarks, auth.rateWindows[42].Count, wantMarks)
					}
					wantAdminLookups := 0
					if identity.authRequired {
						wantAdminLookups = 1
					}
					if store.adminKeyLookups != wantAdminLookups || store.tokenLookups != wantMarks {
						t.Fatalf("credential lookups: admin=%d Token=%d, want %d/%d", store.adminKeyLookups, store.tokenLookups, wantAdminLookups, wantMarks)
					}
					if wantMarks == 1 && store.seenToken != identity.credential {
						t.Fatalf("selected Token = %q, want %q", store.seenToken, identity.credential)
					}
					if denied {
						if brave.calls.Load() != 0 || serper.calls.Load() != 0 || pool.acquired.Load() != 0 || pool.released.Load() != 0 ||
							len(store.logs) != 0 || store.providerReads != 0 || store.cacheReads != 0 || store.cacheWrites != 0 {
							t.Fatal("provider denial reached provider/key/cache/accounting work")
						}
						if response.Body.String() != "{\"error\":{\"message\":\"api token is not allowed to request provider serper\",\"status\":403}}\n" {
							t.Fatalf("provider rejection envelope = %s", response.Body.String())
						}
						return
					}
					wantProviders := test.wantProviders
					if identity.restricted {
						wantProviders = []string{model.ProviderBrave}
					}
					wantCalls := int32(len(wantProviders))
					if pool.acquired.Load() != wantCalls || pool.released.Load() != wantCalls || len(store.logs) != 1 || store.providerReads != 1 || store.cacheReads != 1 || store.cacheWrites != 1 {
						t.Fatalf("selected provider work: acquired=%d released=%d, want %d; logs=%d providers=%d cache reads=%d writes=%d, want 1 each",
							pool.acquired.Load(), pool.released.Load(), wantCalls, len(store.logs), store.providerReads, store.cacheReads, store.cacheWrites)
					}
					for _, adapter := range []*policyTestProvider{brave, serper} {
						wantCount := int32(0)
						if slices.Contains(wantProviders, adapter.name) {
							wantCount = 1
						}
						if adapter.calls.Load() != wantCount {
							t.Fatalf("provider %s executed %d calls, want %d", adapter.name, adapter.calls.Load(), wantCount)
						}
						if wantCount == 0 {
							continue
						}
						received := adapter.request.Load()
						if received == nil || received.Query != "provider policy" || received.Mode != model.SearchModeParallel ||
							received.CompatFormat != route.format || !slices.Equal(received.Providers, wantProviders) ||
							received.ProvidersExplicit != (test.providers != nil) || received.LimitExplicit != (test.providers != nil) || received.Limit != wantLimit {
							t.Fatalf("provider %s request mapping changed: %+v", adapter.name, received)
						}
					}
					entry := store.logs[0]
					if entry.RequestID != requestID || entry.APITokenID != identity.wantTokenID || entry.CompatFormat != string(route.format) ||
						!slices.Equal(entry.Providers, wantProviders) || len(entry.Calls) != len(wantProviders) {
						t.Fatalf("unexpected provider attribution: %+v", entry)
					}
					if entry.Query != "provider policy" || entry.Mode != string(model.SearchModeParallel) || entry.Status != "success" ||
						entry.ErrorMessage != "" || entry.CacheHit || entry.CachePolicy != string(model.CachePolicyDefault) || entry.ResultCount != len(wantProviders) {
						t.Fatalf("unexpected search accounting: %+v", entry)
					}
					seenCalls := map[string]bool{}
					for _, call := range entry.Calls {
						if !slices.Contains(wantProviders, call.ProviderName) || seenCalls[call.ProviderName] || call.ProviderKeyID != 0 ||
							call.AttemptIndex != 1 || call.Status != "success" || call.WillRetry || call.Cached ||
							call.ErrorType != "" || call.ErrorMessage != "" || call.ResultCount != 1 {
							t.Fatalf("unexpected provider call: %+v", call)
						}
						seenCalls[call.ProviderName] = true
						if len(call.Usage) != 1 || call.Usage[0].Unit != "requests" || call.Usage[0].Quantity != 1 {
							t.Fatalf("provider usage changed: %+v", call.Usage)
						}
					}
					var result map[string]json.RawMessage
					if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result) != len(route.responseFields) {
						t.Fatalf("search envelope changed: %s", response.Body.String())
					}
					for _, field := range route.responseFields {
						if result[field] == nil {
							t.Fatalf("search envelope missing %q: %s", field, response.Body.String())
						}
					}
					var results []json.RawMessage
					if err := json.Unmarshal(result[route.resultField], &results); err != nil || len(results) != len(wantProviders) {
						t.Fatalf("unexpected search results: %s", response.Body.String())
					}
					for _, name := range wantProviders {
						if !bytes.Contains(result[route.resultField], []byte("https://"+name+".example/result")) {
							t.Fatalf("selected provider result missing: %s", response.Body.String())
						}
					}
					if route.format == model.CompatFormatNative {
						var meta model.SearchMeta
						if err := json.Unmarshal(result["meta"], &meta); err != nil || meta.RequestID != requestID || meta.CompatFormat != route.format ||
							meta.Mode != model.SearchModeParallel || meta.CacheHit || !slices.Equal(meta.ProvidersQueried, wantProviders) {
							t.Fatalf("native search metadata changed: %s", response.Body.String())
						}
					} else {
						var responseID string
						if err := json.Unmarshal(result[route.requestIDField], &responseID); err != nil || responseID != requestID {
							t.Fatalf("compatibility request identity changed: %s", response.Body.String())
						}
					}
				})
			}
		}
	}
}

// TestMountedSearchProviderValidationOrder keeps decoding and compatibility
// switches ahead of provider policy, including native empty-query precedence.
func TestMountedSearchProviderValidationOrder(t *testing.T) {
	for _, route := range []struct {
		path   string
		compat string
	}{
		{path: "/v1/search"},
		{path: "/v1/compat/tavily/search", compat: "tavily"},
		{path: "/v1/compat/serper/search", compat: "serper"},
		{path: "/v1/compat/openai/responses-search", compat: "openai"},
	} {
		for _, test := range []struct {
			name     string
			body     string
			disabled bool
		}{
			{name: "empty query with excluded provider", body: `{"providers":["serper"]}`},
			{name: "malformed JSON", body: `{"providers":["serper"]`},
			{name: "disabled endpoint before malformed JSON", body: `{"providers":["serper"]`, disabled: true},
		} {
			if test.disabled && route.compat == "" {
				continue
			}
			t.Run(route.path+"/"+test.name, func(t *testing.T) {
				store := &policyTestStore{scopeAuthStore: &scopeAuthStore{
					settings: model.RuntimeSettings{
						APIAuthRequired: true, CompatTavilyEnabled: !test.disabled, CompatSerperEnabled: !test.disabled, CompatOpenAIEnabled: !test.disabled,
					},
					token:         model.APIToken{ID: 42, Scopes: []string{"search"}, AllowedProviders: []string{model.ProviderBrave}, RateLimitPerMin: 10},
					expectedToken: "osr_provider-policy",
				}}
				auth := NewAuthService(store, 0, 0, 0, 0)
				h := NewHandler(store, auth, nil)
				server := NewServer(config.Config{RequestBodyLimitBytes: 1024 * 1024}, &adminAuthTestLogger{})
				server.Mount(h.Mount)
				r := adminTestRequest(http.MethodPost, route.path, test.body, nil)
				r.Header.Set("Authorization", "Bearer osr_provider-policy")
				wantStatus := http.StatusForbidden
				wantMessage := "api token is not allowed to request provider serper"
				if test.disabled {
					wantStatus = http.StatusNotFound
					wantMessage = route.compat + " compatibility endpoint is disabled"
				} else if !json.Valid([]byte(test.body)) {
					wantStatus = http.StatusBadRequest
					wantMessage = "invalid json body"
				} else if route.compat == "" {
					wantStatus = http.StatusBadRequest
					wantMessage = "query is required"
				}
				wantBody, err := json.Marshal(map[string]interface{}{"error": map[string]interface{}{"message": wantMessage, "status": wantStatus}})
				if err != nil {
					t.Fatalf("encode expected rejection: %v", err)
				}
				response := adminTestResponse(t, server.Router(), r, wantStatus)
				if response.Body.String() != string(wantBody)+"\n" {
					t.Fatalf("validation precedence changed: %s", response.Body.String())
				}
				if store.usageMarks != 1 || auth.rateWindows[42].Count != 1 {
					t.Fatal("validation rejection changed Token admission")
				}
				if store.providerReads != 0 || store.cacheReads != 0 || store.cacheWrites != 0 || len(store.logs) != 0 {
					t.Fatal("validation rejection reached orchestration work")
				}
			})
		}
	}
}

// TestRequireSearchTokenProviders checks no-Token and unrestricted no-ops, default
// substitution and denial without changing other normalized request fields.
func TestRequireSearchTokenProviders(t *testing.T) {
	restricted := &model.APIToken{AllowedProviders: []string{model.ProviderBrave}}
	unrestricted := &model.APIToken{}
	for _, test := range []struct {
		name      string
		token     *model.APIToken
		providers []string
		want      []string
		denied    bool
	}{
		{name: "no Token omitted"},
		{name: "no Token empty", providers: []string{}, want: []string{}},
		{name: "no Token explicit", providers: []string{model.ProviderSerper}, want: []string{model.ProviderSerper}},
		{name: "unrestricted omitted", token: unrestricted},
		{name: "unrestricted empty", token: unrestricted, providers: []string{}, want: []string{}},
		{name: "unrestricted explicit", token: unrestricted, providers: []string{model.ProviderSerper}, want: []string{model.ProviderSerper}},
		{name: "restrictive omitted", token: restricted, want: []string{model.ProviderBrave}},
		{name: "restrictive empty", token: restricted, providers: []string{}, want: []string{model.ProviderBrave}},
		{name: "restrictive allowed", token: restricted, providers: []string{model.ProviderBrave}, want: []string{model.ProviderBrave}},
		{name: "restrictive excluded", token: restricted, providers: []string{model.ProviderSerper}, want: []string{model.ProviderSerper}, denied: true},
		{name: "restrictive mixed", token: restricted, providers: []string{model.ProviderBrave, model.ProviderSerper}, want: []string{model.ProviderBrave, model.ProviderSerper}, denied: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dedupe := false
			req := model.SearchRequest{
				Query: "  untouched query  ", Providers: test.providers, ProvidersExplicit: test.providers != nil,
				Mode: model.SearchModeFallback, Limit: 7, LimitExplicit: true, Freshness: "week",
				Dedupe: &dedupe, Rerank: true, Cache: model.CachePolicyRefresh, IncludeRaw: true,
				CompatFormat: model.CompatFormatSerper, Options: map[string]interface{}{"page": 2},
			}
			want := req
			want.Providers = test.want
			r := adminTestRequest(http.MethodPost, "/v1/search", "", nil)
			r.Header.Set("Authorization", "Bearer osr_provider-policy")
			if test.token != nil {
				r = r.WithContext(context.WithValue(r.Context(), apiTokenKey, *test.token))
			}
			response := httptest.NewRecorder()
			if continued := requireSearchTokenProviders(response, r, &req); continued == test.denied {
				t.Fatalf("continue = %t, denied = %t", continued, test.denied)
			}
			if !reflect.DeepEqual(req, want) {
				t.Fatalf("normalized request = %+v, want %+v", req, want)
			}
			if test.denied {
				if response.Code != http.StatusForbidden || response.Body.String() != "{\"error\":{\"message\":\"api token is not allowed to request provider serper\",\"status\":403}}\n" {
					t.Fatalf("provider rejection = %d %s", response.Code, response.Body.String())
				}
			} else if response.Body.Len() != 0 || len(response.Header()) != 0 {
				t.Fatal("provider continuation wrote an HTTP response")
			}
		})
	}
}

type policyTestStore struct {
	*scopeAuthStore
	extractValidationStore
	logs            []model.SearchLogInput
	adminKeyLookups int
	tokenLookups    int
	providerReads   int
	cacheReads      int
	cacheWrites     int
}

// FindAdminAPIKey accepts only the synthetic admin Key and counts selection
// attempts so auth-off requests cannot silently perform credential lookups.
func (s *policyTestStore) FindAdminAPIKey(_ context.Context, credential string) (model.AdminAPIKey, bool, error) {
	s.adminKeyLookups++
	if credential != adminTestKey {
		return model.AdminAPIKey{}, false, nil
	}
	return model.AdminAPIKey{KeyPrefix: "oak_synthetic"}, true, nil
}

// FindAPIToken counts ordinary-Token lookups and delegates to the scope fixture's
// optional expected-credential check.
func (s *policyTestStore) FindAPIToken(ctx context.Context, credential string) (model.APIToken, error) {
	s.tokenLookups++
	return s.scopeAuthStore.FindAPIToken(ctx, credential)
}

// RuntimeSettings exposes the scope fixture's configured auth and routing choices
// to both middleware and orchestration.
func (s *policyTestStore) RuntimeSettings(ctx context.Context) (model.RuntimeSettings, error) {
	return s.scopeAuthStore.RuntimeSettings(ctx)
}

// ListProviders counts orchestration configuration reads and enables both test
// providers so Token policy, not disabled-provider filtering, determines access.
func (s *policyTestStore) ListProviders(context.Context) ([]model.ProviderConfig, error) {
	s.providerReads++
	return []model.ProviderConfig{
		{Name: model.ProviderBrave, Enabled: true, AvailableKeys: 1},
		{Name: model.ProviderSerper, Enabled: true, AvailableKeys: 1},
	}, nil
}

// RecordSearchLog captures completed search attribution and per-provider usage
// without persisting test data.
func (s *policyTestStore) RecordSearchLog(_ context.Context, input model.SearchLogInput) error {
	s.logs = append(s.logs, input)
	return nil
}

// GetCache counts cache access but always misses so permitted requests exercise
// provider execution and usage accounting.
func (s *policyTestStore) GetCache(context.Context, string) ([]byte, bool, error) {
	s.cacheReads++
	return nil, false, nil
}

// SetCache records write attempts without retaining payloads between fixture calls.
func (s *policyTestStore) SetCache(context.Context, string, []byte, int) error {
	s.cacheWrites++
	return nil
}

type policyTestProvider struct {
	name       string
	calls      atomic.Int32
	request    atomic.Pointer[model.SearchRequest]
	searchHook func()
}

// Name supplies the configured registry and result-attribution identity.
func (p *policyTestProvider) Name() string {
	return p.name
}

// Search records the normalized request and returns one attributed result and
// request unit; the optional hook lets MCP fixtures cancel after execution starts.
func (p *policyTestProvider) Search(_ context.Context, req model.SearchRequest, _ model.APIKey) (model.ProviderResponse, error) {
	p.calls.Add(1)
	p.request.Store(&req)
	if p.searchHook != nil {
		p.searchHook()
	}
	return model.ProviderResponse{
		Results: []model.SearchResult{{Provider: p.name, Title: p.name, URL: "https://" + p.name + ".example/result"}},
		Usage:   []model.UsageMeasurement{{Unit: "requests", Quantity: 1}},
	}, nil
}

// HealthCheck succeeds without network I/O; provider availability is fixture data.
func (*policyTestProvider) HealthCheck(context.Context, model.APIKey) error {
	return nil
}

type policyTestKeyPool struct {
	acquired atomic.Int32
	released atomic.Int32
}

// Acquire counts leases and releases while returning a provider-attributed key
// with no persistent identity or secret.
func (p *policyTestKeyPool) Acquire(_ context.Context, name string) (model.APIKey, func(bool, error), error) {
	p.acquired.Add(1)
	// A synthetic zero ID keeps these API fixtures out of quota-refresh I/O.
	return model.APIKey{ProviderName: name}, func(bool, error) {
		p.released.Add(1)
	}, nil
}

// TestRemovedPublicMetadataRoutes keeps removed paths inert across credentials
// and auth modes while preserving authenticated management reads of global data.
func TestRemovedPublicMetadataRoutes(t *testing.T) {
	for _, authRequired := range []bool{true, false} {
		t.Run(fmt.Sprintf("auth=%t", authRequired), func(t *testing.T) {
			f := newAdminAuthFixture(t, config.Config{})
			cookie := f.login(t)
			f.store.settings.APIAuthRequired = authRequired
			store := &metadataRouteTestStore{adminAuthTestStore: f.store}
			h := NewHandler(store, f.auth, nil)
			server := NewServer(config.Config{}, f.log)
			server.Mount(h.Mount)
			for _, path := range []string{"/v1/providers", "/v1/usage/summary", "/v1/providers/", "/v1/usage/summary/"} {
				for _, credential := range []string{"", adminTestToken, adminTestKey, "invalid", "cookie"} {
					for _, header := range []string{"Authorization", "X-API-Key"} {
						r := adminTestRequest(http.MethodGet, path, "", nil)
						if credential == "cookie" {
							r.AddCookie(cookie)
						} else if credential != "" {
							value := credential
							if header == "Authorization" {
								value = "Bearer " + value
							}
							r.Header.Set(header, value)
						}
						response := adminTestResponse(t, server.Router(), r, http.StatusNotFound)
						if strings.Contains(response.Body.String(), "private-config-sentinel") || strings.Contains(response.Body.String(), "requests_total") {
							t.Fatal("removed public route returned global data")
						}
					}
				}
			}
			if store.providerReads != 0 || store.usageReads != 0 || len(f.store.keyLookups) != 0 || len(f.store.tokenLookups) != 0 || f.store.usageMarks != 0 {
				t.Fatal("removed route still reached authentication or global-data handlers")
			}
			for _, path := range []string{"/api/admin/providers", "/api/admin/usage/summary"} {
				adminTestResponse(t, server.Router(), adminTestRequest(http.MethodGet, path, "", nil), http.StatusUnauthorized)
				r := adminTestRequest(http.MethodGet, path, "", nil)
				r.Header.Set("Authorization", "Bearer "+adminTestToken)
				adminTestResponse(t, server.Router(), r, http.StatusUnauthorized)
				for _, credential := range []string{"cookie", "Authorization", "X-API-Key"} {
					r := adminTestRequest(http.MethodGet, path, "", nil)
					if credential == "cookie" {
						r.AddCookie(cookie)
					} else {
						value := adminTestKey
						if credential == "Authorization" {
							value = "Bearer " + value
						}
						r.Header.Set(credential, value)
					}
					response := adminTestResponse(t, server.Router(), r, http.StatusOK)
					if strings.HasSuffix(path, "/providers") {
						if !strings.Contains(response.Body.String(), "private-config-sentinel") {
							t.Fatal("admin provider configuration was removed")
						}
					} else {
						var summary model.UsageSummary
						if err := json.Unmarshal(response.Body.Bytes(), &summary); err != nil || summary.RequestsTotal != 73 {
							t.Fatalf("admin aggregate changed: %s", response.Body.String())
						}
					}
				}
			}
			if store.providerReads != 3 || store.usageReads != 3 {
				t.Fatalf("admin data reads: providers=%d usage=%d", store.providerReads, store.usageReads)
			}
		})
	}
}

type metadataRouteTestStore struct {
	*adminAuthTestStore
	providerReads int
	usageReads    int
}

// ListProviders counts global configuration reads and includes a private sentinel
// that must remain absent from removed public routes.
func (s *metadataRouteTestStore) ListProviders(context.Context) ([]model.ProviderConfig, error) {
	s.providerReads++
	return []model.ProviderConfig{{Name: model.ProviderBrave, Settings: map[string]interface{}{"proxy_url": "private-config-sentinel"}}}, nil
}

// UsageSummary returns a recognizable global aggregate and counts management reads.
func (s *metadataRouteTestStore) UsageSummary(context.Context) (model.UsageSummary, error) {
	s.usageReads++
	return model.UsageSummary{RequestsTotal: 73}, nil
}
