package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vihor3/searchmeld/backend/internal/config"
	"github.com/vihor3/searchmeld/backend/internal/model"
	"github.com/vihor3/searchmeld/backend/internal/provider"
	"github.com/vihor3/searchmeld/backend/internal/search"
)

func TestMountedCompatSearchProviderPolicy(t *testing.T) {
	for _, route := range []struct {
		path        string
		queryField  string
		resultField string
		format      model.CompatFormat
	}{
		{path: "/v1/compat/tavily/search", queryField: "query", resultField: "results", format: model.CompatFormatTavily},
		{path: "/v1/compat/serper/search", queryField: "q", resultField: "organic", format: model.CompatFormatSerper},
		{path: "/v1/compat/openai/responses-search", queryField: "input", resultField: "search_results", format: model.CompatFormatOpenAI},
	} {
		for _, test := range []struct {
			name      string
			providers []string
			denied    bool
		}{
			{name: "excluded global default"},
			{name: "empty explicit providers", providers: []string{}},
			{name: "allowed explicit", providers: []string{model.ProviderBrave}},
			{name: "excluded explicit", providers: []string{model.ProviderSerper}, denied: true},
			{name: "mixed explicit", providers: []string{model.ProviderBrave, model.ProviderSerper}, denied: true},
		} {
			t.Run(route.path+"/"+test.name, func(t *testing.T) {
				allowed := &policyTestProvider{name: model.ProviderBrave}
				excluded := &policyTestProvider{name: model.ProviderSerper}
				pool := &policyTestKeyPool{}
				store := &policyTestStore{
					scopeAuthStore: &scopeAuthStore{
						settings: model.RuntimeSettings{
							APIAuthRequired: true, CompatTavilyEnabled: true, CompatSerperEnabled: true, CompatOpenAIEnabled: true,
							DefaultProviders: []string{model.ProviderSerper}, CacheEnabled: true,
						},
						token:         model.APIToken{ID: 42, Scopes: []string{"search"}, AllowedProviders: []string{model.ProviderBrave}},
						expectedToken: "osr_provider-policy",
					},
				}
				orchestrator := search.NewOrchestrator(provider.NewRegistry(allowed, excluded), pool, store)
				h := NewHandler(store, NewAuthService(store, 0, 0, 0, 0), orchestrator)
				server := NewServer(config.Config{RequestBodyLimitBytes: 1024 * 1024}, &adminAuthTestLogger{})
				server.Mount(h.Mount)
				body := map[string]interface{}{route.queryField: "provider policy", "mode": "parallel"}
				if test.providers != nil {
					body["providers"] = test.providers
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatalf("encode request: %v", err)
				}
				r := adminTestRequest(http.MethodPost, route.path, string(encoded), nil)
				r.Header.Set("Authorization", "Bearer osr_provider-policy")
				wantStatus := http.StatusOK
				if test.denied {
					wantStatus = http.StatusForbidden
				}
				response := adminTestResponse(t, server.Router(), r, wantStatus)
				if excluded.calls.Load() != 0 {
					t.Fatalf("excluded provider executed %d calls", excluded.calls.Load())
				}
				if store.usageMarks != 1 {
					t.Fatalf("token admission marks = %d, want 1", store.usageMarks)
				}
				if test.denied {
					if allowed.calls.Load() != 0 || pool.acquired.Load() != 0 || len(store.logs) != 0 || store.cacheReads != 0 {
						t.Fatal("provider denial reached provider/key/cache/accounting work")
					}
					var rejection struct {
						Error struct {
							Status  int    `json:"status"`
							Message string `json:"message"`
						} `json:"error"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &rejection); err != nil || rejection.Error.Status != http.StatusForbidden || !strings.Contains(rejection.Error.Message, "provider serper") {
						t.Fatalf("provider rejection envelope = %s", response.Body.String())
					}
					return
				}
				if allowed.calls.Load() != 1 || pool.acquired.Load() != 1 || len(store.logs) != 1 || store.cacheReads != 1 {
					t.Fatal("allowed provider did not execute exactly once with normal accounting")
				}
				entry := store.logs[0]
				if entry.APITokenID != 42 || entry.CompatFormat != string(route.format) || len(entry.Providers) != 1 || entry.Providers[0] != model.ProviderBrave || len(entry.Calls) != 1 || entry.Calls[0].ProviderName != model.ProviderBrave {
					t.Fatalf("unexpected provider attribution: %+v", entry)
				}
				if len(entry.Calls[0].Usage) != 1 || entry.Calls[0].Usage[0].Quantity != 1 {
					t.Fatal("allowed provider usage was not preserved")
				}
				var result map[string]json.RawMessage
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result[route.resultField] == nil {
					t.Fatalf("compatibility envelope changed: %s", response.Body.String())
				}
				if !bytes.Contains(result[route.resultField], []byte("https://brave.example/result")) {
					t.Fatalf("allowed result missing: %s", response.Body.String())
				}
			})
		}
	}
}

type policyTestStore struct {
	*scopeAuthStore
	extractValidationStore
	logs       []model.SearchLogInput
	cacheReads int
}

func (s *policyTestStore) RuntimeSettings(ctx context.Context) (model.RuntimeSettings, error) {
	return s.scopeAuthStore.RuntimeSettings(ctx)
}

func (*policyTestStore) ListProviders(context.Context) ([]model.ProviderConfig, error) {
	return []model.ProviderConfig{
		{Name: model.ProviderBrave, Enabled: true, AvailableKeys: 1},
		{Name: model.ProviderSerper, Enabled: true, AvailableKeys: 1},
	}, nil
}

func (s *policyTestStore) RecordSearchLog(_ context.Context, input model.SearchLogInput) error {
	s.logs = append(s.logs, input)
	return nil
}

func (s *policyTestStore) GetCache(context.Context, string) ([]byte, bool, error) {
	s.cacheReads++
	return nil, false, nil
}

type policyTestProvider struct {
	name       string
	calls      atomic.Int32
	searchHook func()
}

func (p *policyTestProvider) Name() string {
	return p.name
}

func (p *policyTestProvider) Search(context.Context, model.SearchRequest, model.APIKey) (model.ProviderResponse, error) {
	p.calls.Add(1)
	if p.searchHook != nil {
		p.searchHook()
	}
	return model.ProviderResponse{
		Results: []model.SearchResult{{Provider: p.name, Title: p.name, URL: "https://" + p.name + ".example/result"}},
		Usage:   []model.UsageMeasurement{{Unit: "requests", Quantity: 1}},
	}, nil
}

func (*policyTestProvider) HealthCheck(context.Context, model.APIKey) error {
	return nil
}

type policyTestKeyPool struct {
	acquired atomic.Int32
}

func (p *policyTestKeyPool) Acquire(_ context.Context, name string) (model.APIKey, func(bool, error), error) {
	p.acquired.Add(1)
	// A synthetic zero ID keeps these API fixtures out of quota-refresh I/O.
	return model.APIKey{ProviderName: name}, func(bool, error) {}, nil
}

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

func (s *metadataRouteTestStore) ListProviders(context.Context) ([]model.ProviderConfig, error) {
	s.providerReads++
	return []model.ProviderConfig{{Name: model.ProviderBrave, Settings: map[string]interface{}{"proxy_url": "private-config-sentinel"}}}, nil
}

func (s *metadataRouteTestStore) UsageSummary(context.Context) (model.UsageSummary, error) {
	s.usageReads++
	return model.UsageSummary{RequestsTotal: 73}, nil
}
