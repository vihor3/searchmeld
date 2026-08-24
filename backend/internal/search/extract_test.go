package search

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/one-search/one-search/backend/internal/model"
	"github.com/one-search/one-search/backend/internal/provider"
)

type extractTestProvider struct {
	name      string
	batchSize int
	mu        *sync.Mutex
	requests  *[][]string
	extract   func(model.ExtractRequest) (model.ExtractProviderResponse, error)
}

func (p extractTestProvider) ExtractBatchSize() int {
	return p.batchSize
}

func (p extractTestProvider) Name() string {
	return p.name
}

func (p extractTestProvider) Search(ctx context.Context, req model.SearchRequest, key model.APIKey) (model.ProviderResponse, error) {
	return model.ProviderResponse{}, nil
}

func (p extractTestProvider) Extract(ctx context.Context, req model.ExtractRequest, key model.APIKey) (model.ExtractProviderResponse, error) {
	if p.requests != nil {
		if p.mu != nil {
			p.mu.Lock()
			defer p.mu.Unlock()
		}
		*p.requests = append(*p.requests, append([]string(nil), req.URLs...))
	}
	if p.extract == nil {
		return model.ExtractProviderResponse{}, nil
	}
	return p.extract(req)
}

func (p extractTestProvider) HealthCheck(ctx context.Context, key model.APIKey) error {
	return nil
}

func TestApplyExtractDefaultsValidatesAndNormalizes(t *testing.T) {
	tests := []struct {
		name    string
		request model.ExtractRequest
	}{
		{name: "missing URLs", request: model.ExtractRequest{}},
		{name: "invalid URL", request: model.ExtractRequest{URLs: []string{"ftp://example.com/file"}}},
		{name: "credentials", request: model.ExtractRequest{URLs: []string{"https://user:secret@example.com"}}},
		{name: "unsupported mode", request: model.ExtractRequest{URLs: []string{"https://example.com"}, Mode: "aggregate"}},
		{name: "unsupported format", request: model.ExtractRequest{URLs: []string{"https://example.com"}, Format: "pdf"}},
		{name: "unsupported provider", request: model.ExtractRequest{URLs: []string{"https://example.com"}, Providers: []string{model.ProviderSerper}, ProvidersExplicit: true}},
		{name: "too many chunks", request: model.ExtractRequest{URLs: []string{"https://example.com"}, ChunksPerSource: 6}},
		{name: "chunks without query", request: model.ExtractRequest{URLs: []string{"https://example.com"}, ChunksPerSource: 3}},
		{name: "explicit zero chunks", request: model.ExtractRequest{URLs: []string{"https://example.com"}, Query: "focus", ChunksPerSourceSet: true}},
		{name: "private IP", request: model.ExtractRequest{URLs: []string{"http://169.254.169.254/latest/meta-data"}}},
		{name: "this-network IP", request: model.ExtractRequest{URLs: []string{"http://0.0.0.1/"}}},
		{name: "reserved IP", request: model.ExtractRequest{URLs: []string{"http://192.0.2.1/"}}},
		{name: "NAT64 loopback", request: model.ExtractRequest{URLs: []string{"http://[64:ff9b::7f00:1]/"}}},
		{name: "6to4 loopback", request: model.ExtractRequest{URLs: []string{"http://[2002:7f00:1::]/"}}},
		{name: "private hostname", request: model.ExtractRequest{URLs: []string{"http://database.internal/status"}}},
		{name: "unsupported Tavily format", request: model.ExtractRequest{URLs: []string{"https://example.com"}, Format: model.ExtractFormatHTML, CompatFormat: model.CompatFormatTavily}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := applyExtractDefaults(test.request, model.RuntimeSettings{})
			var validationErr *ExtractValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %v, want ExtractValidationError", err)
			}
		})
	}

	request, err := applyExtractDefaults(model.ExtractRequest{
		URLs: []string{" https://Example.com/a#one ", "https://example.com/a#two"},
	}, model.RuntimeSettings{DefaultProviders: []string{model.ProviderSerper, model.ProviderJina}})
	if err != nil {
		t.Fatalf("applyExtractDefaults returned error: %v", err)
	}
	if got, want := request.Mode, model.SearchModeFallback; got != want {
		t.Fatalf("Mode = %q, want %q", got, want)
	}
	if got, want := request.Format, model.ExtractFormatMarkdown; got != want {
		t.Fatalf("Format = %q, want %q", got, want)
	}
	if got, want := request.Providers, []string{model.ProviderJina}; !stringSlicesEqual(got, want) {
		t.Fatalf("Providers = %v, want %v", got, want)
	}
	if got, want := len(request.URLs), 1; got != want {
		t.Fatalf("URLs length = %d, want %d", got, want)
	}

	// Token provider constraints are applied before orchestration but do not
	// make the providers user-explicit. Search-only entries must be ignored.
	tokenConstrained, err := applyExtractDefaults(model.ExtractRequest{
		URLs:      []string{"https://example.com"},
		Providers: append([]string(nil), model.DefaultProviders...),
	}, model.RuntimeSettings{})
	if err != nil {
		t.Fatalf("token-constrained defaults returned error: %v", err)
	}
	if got, want := tokenConstrained.Providers, model.ExtractProviders; !stringSlicesEqual(got, want) {
		t.Fatalf("token-constrained Providers = %v, want %v", got, want)
	}

	noExtractPermission, err := applyExtractDefaults(model.ExtractRequest{
		URLs:      []string{"https://example.com"},
		Providers: []string{model.ProviderSerper},
	}, model.RuntimeSettings{})
	if err != nil {
		t.Fatalf("non-explicit search-only constraint returned error: %v", err)
	}
	if len(noExtractPermission.Providers) != 0 {
		t.Fatalf("Providers = %v, want none when token permits no extract provider", noExtractPermission.Providers)
	}

	searchOnlyDefaults, err := applyExtractDefaults(
		model.ExtractRequest{URLs: []string{"https://example.com"}},
		model.RuntimeSettings{DefaultProviders: []string{model.ProviderSerper, model.ProviderBrave}},
	)
	if err != nil {
		t.Fatalf("search-only runtime defaults returned error: %v", err)
	}
	if len(searchOnlyDefaults.Providers) != 0 {
		t.Fatalf("Providers = %v, want no implicit extract providers outside runtime defaults", searchOnlyDefaults.Providers)
	}

	privateTarget, err := applyExtractDefaults(
		model.ExtractRequest{URLs: []string{"http://127.0.0.1:8080/page"}},
		model.RuntimeSettings{AllowPrivateExtractTargets: true},
	)
	if err != nil || len(privateTarget.URLs) != 1 {
		t.Fatalf("trusted private target should be allowed: request=%+v err=%v", privateTarget, err)
	}

	for _, timeout := range []float64{0, 0.5, 60.1} {
		_, err := applyExtractDefaults(model.ExtractRequest{
			URLs:         []string{"https://example.com"},
			CompatFormat: model.CompatFormatTavily,
			Options:      map[string]interface{}{"timeout": timeout},
		}, model.RuntimeSettings{})
		if err == nil {
			t.Fatalf("Tavily timeout %v should be rejected", timeout)
		}
	}
	if _, err := applyExtractDefaults(model.ExtractRequest{
		URLs:         []string{"https://example.com"},
		CompatFormat: model.CompatFormatTavily,
		Options:      map[string]interface{}{"timeout": 12.5},
	}, model.RuntimeSettings{}); err != nil {
		t.Fatalf("valid Tavily float timeout rejected: %v", err)
	}
}

func TestExtractDNSValidationRejectsPrivateResolution(t *testing.T) {
	orchestrator := &Orchestrator{lookupIPAddr: func(ctx context.Context, hostname string) ([]net.IPAddr, error) {
		switch hostname {
		case "public.example":
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		case "mixed.example":
			return []net.IPAddr{{IP: net.ParseIP("8.8.4.4")}, {IP: net.ParseIP("10.0.0.8")}}, nil
		default:
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}
	}}
	if err := orchestrator.validateResolvedExtractTargets(context.Background(), []string{"https://public.example/page"}); err != nil {
		t.Fatalf("public resolution rejected: %v", err)
	}
	for _, target := range []string{
		"http://redis:6379/",
		"http://127.0.0.1.nip.io/",
		"http://127.0.0.0x1/",
		"https://mixed.example/",
	} {
		if err := orchestrator.validateResolvedExtractTargets(context.Background(), []string{target}); err == nil {
			t.Fatalf("private DNS target %q should be rejected", target)
		}
	}
}

func TestExtractTimeoutBudgetAccountsForBatchWavesAndTavilyTimeout(t *testing.T) {
	req := model.ExtractRequest{
		URLs:         make([]string, 20),
		Providers:    []string{model.ProviderJina},
		Mode:         model.SearchModeSingle,
		CompatFormat: model.CompatFormatNative,
	}
	if got := effectiveExtractRequestTimeoutMS(20000, req, map[string]int{model.ProviderJina: 15000}); got != 301000 {
		t.Fatalf("serial Jina timeout budget = %d, want 301000", got)
	}
	req.Providers = []string{model.ProviderFirecrawl}
	if got := effectiveExtractRequestTimeoutMS(20000, req, map[string]int{model.ProviderFirecrawl: 30000}); got != 601000 {
		t.Fatalf("serial Firecrawl timeout budget = %d, want 601000", got)
	}
	req.Providers = []string{model.ProviderExa, model.ProviderJina, model.ProviderTavily, model.ProviderFirecrawl}
	req.Mode = model.SearchModeFallback
	if got := effectiveExtractRequestTimeoutMS(20000, req, map[string]int{
		model.ProviderExa:       12000,
		model.ProviderJina:      15000,
		model.ProviderTavily:    15000,
		model.ProviderFirecrawl: 30000,
	}); got != 931000 {
		t.Fatalf("max fallback timeout budget = %d, want 931000", got)
	}

	tavilyReq := model.ExtractRequest{
		URLs:         []string{"https://example.com"},
		Providers:    []string{model.ProviderTavily},
		Mode:         model.SearchModeSingle,
		CompatFormat: model.CompatFormatTavily,
		Options:      map[string]interface{}{"timeout": 60.0},
	}
	timeouts := adjustedExtractProviderTimeouts(tavilyReq, map[string]int{model.ProviderTavily: 15000})
	if timeouts[model.ProviderTavily] != 61000 {
		t.Fatalf("Tavily provider timeout = %d, want 61000", timeouts[model.ProviderTavily])
	}
	if got := effectiveExtractRequestTimeoutMS(20000, tavilyReq, timeouts); got != 62000 {
		t.Fatalf("Tavily request timeout budget = %d, want 62000", got)
	}
	tavilyReq.URLs = make([]string, 20)
	tavilyReq.Providers = []string{model.ProviderExa, model.ProviderJina, model.ProviderTavily, model.ProviderFirecrawl}
	tavilyReq.Mode = model.SearchModeFallback
	timeouts = adjustedExtractProviderTimeouts(tavilyReq, map[string]int{
		model.ProviderExa:       12000,
		model.ProviderJina:      15000,
		model.ProviderTavily:    15000,
		model.ProviderFirecrawl: 30000,
	})
	for _, providerName := range tavilyReq.Providers {
		if timeouts[providerName] != 61000 {
			t.Fatalf("%s provider timeout = %d, want 61000", providerName, timeouts[providerName])
		}
	}
	if got := effectiveExtractRequestTimeoutMS(20000, tavilyReq, timeouts); got != 62000 {
		t.Fatalf("Tavily aggregate timeout budget = %d, want caller-visible 62000", got)
	}
}

func TestNormalizeExtractResultsOnlyAcceptsRequestedURLs(t *testing.T) {
	req := model.ExtractRequest{URLs: []string{
		"https://Example.com",
		"https://example.com/article",
	}}
	results, failures := normalizeExtractProviderResults([]model.ExtractResult{
		{URL: "https://example.com/", Content: "root"},
		{URL: "https://unrequested.example/page", Content: "extra"},
	}, model.ProviderTavily, req)
	if len(results) != 1 || results[0].URL != req.URLs[0] || results[0].Content != "root" {
		t.Fatalf("normalized results = %+v, want only the requested root URL", results)
	}
	if len(failures) != 1 || failures[0].ErrorType != provider.ErrorTypeInvalidResponse {
		t.Fatalf("failures = %+v, want one unrequested-URL failure", failures)
	}
	if extractURLKey("https://example.com:443") != extractURLKey("https://example.com/") {
		t.Fatal("default HTTPS port and root slash should normalize to the same URL key")
	}

	single := model.ExtractRequest{URLs: []string{"https://example.com/redirecting"}}
	results, failures = normalizeExtractProviderResults([]model.ExtractResult{{
		URL: "https://cdn.example.net/canonical", Content: "redirected",
	}}, model.ProviderTavily, single)
	if len(failures) != 0 || len(results) != 1 || results[0].URL != single.URLs[0] {
		t.Fatalf("single canonical result was not correlated: results=%+v failures=%+v", results, failures)
	}
}

func TestExtractFallbackOnlyPassesUnresolvedURLs(t *testing.T) {
	firstURL := "https://example.com/first"
	secondURL := "https://example.com/second"
	var exaRequests [][]string
	var jinaRequests [][]string
	registry := provider.NewRegistry(
		extractTestProvider{
			name:     model.ProviderExa,
			requests: &exaRequests,
			extract: func(req model.ExtractRequest) (model.ExtractProviderResponse, error) {
				return model.ExtractProviderResponse{
					Results:       []model.ExtractResult{{URL: firstURL, Content: "from exa"}},
					FailedResults: []model.ExtractFailure{{URL: secondURL, Error: "blocked"}},
				}, nil
			},
		},
		extractTestProvider{
			name:     model.ProviderJina,
			requests: &jinaRequests,
			extract: func(req model.ExtractRequest) (model.ExtractProviderResponse, error) {
				return model.ExtractProviderResponse{Results: []model.ExtractResult{{URL: secondURL, Content: "from jina"}}}, nil
			},
		},
	)
	store := &extractTestStore{orchestratorTestStore: orchestratorTestStore{
		settings: model.RuntimeSettings{DefaultProviders: []string{model.ProviderExa, model.ProviderJina}, RequestTimeoutMS: 1000, AllowPrivateExtractTargets: true},
		providers: []model.ProviderConfig{
			{Name: model.ProviderExa, Enabled: true, Priority: 1, Weight: 1},
			{Name: model.ProviderJina, Enabled: true, Priority: 2, Weight: 1},
		},
	}}
	orchestrator := NewOrchestrator(registry, &orchestratorTestKeyPool{}, store)

	response, err := orchestrator.Extract(context.Background(), model.ExtractRequest{URLs: []string{firstURL, secondURL}}, "extract-1", 7)
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if got, want := exaRequests, [][]string{{firstURL, secondURL}}; !nestedStringSlicesEqual(got, want) {
		t.Fatalf("Exa requests = %v, want %v", got, want)
	}
	if got, want := jinaRequests, [][]string{{secondURL}}; !nestedStringSlicesEqual(got, want) {
		t.Fatalf("Jina requests = %v, want %v", got, want)
	}
	if len(response.Results) != 2 || response.Results[0].URL != firstURL || response.Results[1].URL != secondURL {
		t.Fatalf("Results = %+v, want both URLs in request order", response.Results)
	}
	if len(response.FailedResults) != 0 {
		t.Fatalf("FailedResults = %+v, want none", response.FailedResults)
	}
	if got, want := response.Meta.ProvidersQueried, []string{model.ProviderExa, model.ProviderJina}; !stringSlicesEqual(got, want) {
		t.Fatalf("ProvidersQueried = %v, want %v", got, want)
	}
	if len(store.logs) != 1 || store.logs[0].Operation != "extract" || store.logs[0].APITokenID != 7 {
		t.Fatalf("logs = %+v, want one extract log for token 7", store.logs)
	}
}

func TestExtractFallbackContinuesAfterEmptyContent(t *testing.T) {
	targetURL := "https://example.com/empty-first"
	secondCalled := false
	registry := provider.NewRegistry(
		extractTestProvider{name: model.ProviderExa, extract: func(req model.ExtractRequest) (model.ExtractProviderResponse, error) {
			return model.ExtractProviderResponse{Results: []model.ExtractResult{{URL: targetURL}}}, nil
		}},
		extractTestProvider{name: model.ProviderJina, extract: func(req model.ExtractRequest) (model.ExtractProviderResponse, error) {
			secondCalled = true
			return model.ExtractProviderResponse{Results: []model.ExtractResult{{URL: targetURL, Content: "fallback content"}}}, nil
		}},
	)
	store := &orchestratorTestStore{
		settings: model.RuntimeSettings{RequestTimeoutMS: 1000, AllowPrivateExtractTargets: true},
		providers: []model.ProviderConfig{
			{Name: model.ProviderExa, Enabled: true, Priority: 1},
			{Name: model.ProviderJina, Enabled: true, Priority: 2},
		},
	}
	orchestrator := NewOrchestrator(registry, &orchestratorTestKeyPool{}, store)
	response, err := orchestrator.Extract(context.Background(), model.ExtractRequest{
		URLs:              []string{targetURL},
		Providers:         []string{model.ProviderExa, model.ProviderJina},
		ProvidersExplicit: true,
	}, "extract-empty", 0)
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if !secondCalled || len(response.Results) != 1 || response.Results[0].Content != "fallback content" {
		t.Fatalf("response = %+v, secondCalled = %v", response, secondCalled)
	}
}

func TestExtractSingleURLAdapterMetersEveryUpstreamRequest(t *testing.T) {
	urls := []string{"https://example.com/one", "https://example.com/two"}
	var requests [][]string
	registry := provider.NewRegistry(extractTestProvider{
		name:      model.ProviderJina,
		batchSize: 1,
		requests:  &requests,
		extract: func(req model.ExtractRequest) (model.ExtractProviderResponse, error) {
			return model.ExtractProviderResponse{
				Results: []model.ExtractResult{{URL: req.URLs[0], Content: "content"}},
				Usage:   []model.UsageMeasurement{{Unit: "credits", Quantity: 1}},
			}, nil
		},
	})
	store := &extractTestStore{orchestratorTestStore: orchestratorTestStore{
		settings:  model.RuntimeSettings{RequestTimeoutMS: 1000, AllowPrivateExtractTargets: true},
		providers: []model.ProviderConfig{{Name: model.ProviderJina, Enabled: true, Settings: map[string]interface{}{"key_retry_count": 0}}},
	}}
	keyPool := &orchestratorTestKeyPool{}
	orchestrator := NewOrchestrator(registry, keyPool, store)
	response, err := orchestrator.Extract(context.Background(), model.ExtractRequest{
		URLs:              urls,
		Providers:         []string{model.ProviderJina},
		ProvidersExplicit: true,
		Mode:              model.SearchModeSingle,
	}, "extract-metered", 0)
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if got, want := requests, [][]string{{urls[0]}, {urls[1]}}; !nestedStringSlicesEqual(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	if len(keyPool.acquired) != 2 || len(response.Results) != 2 {
		t.Fatalf("acquired = %v, response = %+v", keyPool.acquired, response)
	}
	if len(store.logs) != 1 || len(store.logs[0].Calls) != 2 {
		t.Fatalf("logs = %+v, want two independently metered calls", store.logs)
	}
	if len(response.Usage) != 2 || response.Usage[0].Quantity+response.Usage[1].Quantity != 2 {
		t.Fatalf("response usage = %+v, want both upstream calls", response.Usage)
	}
}

func TestExtractSingleURLAdapterExecutesSequentially(t *testing.T) {
	urls := []string{"https://example.com/one", "https://example.com/two", "https://example.com/three", "https://example.com/four"}
	var mu sync.Mutex
	active := 0
	maxActive := 0
	registry := provider.NewRegistry(extractTestProvider{
		name:      model.ProviderJina,
		batchSize: 1,
		extract: func(req model.ExtractRequest) (model.ExtractProviderResponse, error) {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			return model.ExtractProviderResponse{Results: []model.ExtractResult{{URL: req.URLs[0], Content: "content"}}}, nil
		},
	})
	store := &orchestratorTestStore{
		settings: model.RuntimeSettings{RequestTimeoutMS: 1000, AllowPrivateExtractTargets: true},
		providers: []model.ProviderConfig{{
			Name: model.ProviderJina, Enabled: true, Settings: map[string]interface{}{"key_retry_count": 0},
		}},
	}
	orchestrator := NewOrchestrator(registry, &orchestratorTestKeyPool{}, store)
	response, err := orchestrator.Extract(context.Background(), model.ExtractRequest{
		URLs: urls, Providers: []string{model.ProviderJina}, ProvidersExplicit: true, Mode: model.SearchModeSingle,
	}, "extract-sequential", 0)
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if len(response.Results) != len(urls) || maxActive != 1 {
		t.Fatalf("results=%d maxActive=%d, want strictly sequential upstream calls", len(response.Results), maxActive)
	}
}

func TestExtractSingleURLAdapterPreservesPartialResultsOnSystemicError(t *testing.T) {
	urls := []string{"https://example.com/one", "https://example.com/two", "https://example.com/three"}
	var requests [][]string
	registry := provider.NewRegistry(extractTestProvider{
		name:      model.ProviderFirecrawl,
		batchSize: 1,
		requests:  &requests,
		extract: func(req model.ExtractRequest) (model.ExtractProviderResponse, error) {
			if req.URLs[0] == urls[1] {
				return model.ExtractProviderResponse{}, &provider.Error{Type: provider.ErrorTypeRateLimited, Message: "limited"}
			}
			return model.ExtractProviderResponse{Results: []model.ExtractResult{{URL: req.URLs[0], Content: "paid content"}}}, nil
		},
	})
	store := &orchestratorTestStore{
		settings: model.RuntimeSettings{RequestTimeoutMS: 1000, AllowPrivateExtractTargets: true},
		providers: []model.ProviderConfig{{
			Name: model.ProviderFirecrawl, Enabled: true, Settings: map[string]interface{}{"key_retry_count": 0},
		}},
	}
	keyPool := &orchestratorTestKeyPool{}
	orchestrator := NewOrchestrator(registry, keyPool, store)
	response, err := orchestrator.Extract(context.Background(), model.ExtractRequest{
		URLs:              urls,
		Providers:         []string{model.ProviderFirecrawl},
		ProvidersExplicit: true,
		Mode:              model.SearchModeSingle,
	}, "extract-partial", 0)
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if len(response.Results) != 1 || response.Results[0].URL != urls[0] {
		t.Fatalf("Results = %+v, want first paid result preserved", response.Results)
	}
	if len(requests) != 2 || len(keyPool.acquired) != 2 {
		t.Fatalf("requests = %v, acquired = %v; third URL must not be retried after systemic error", requests, keyPool.acquired)
	}
	if len(response.FailedResults) != 2 {
		t.Fatalf("FailedResults = %+v, want unresolved second and third URLs", response.FailedResults)
	}
}

func TestExtractParallelMergesProviderAttribution(t *testing.T) {
	targetURL := "https://example.com/page?version=1"
	registry := provider.NewRegistry(
		extractTestProvider{name: model.ProviderExa, extract: func(req model.ExtractRequest) (model.ExtractProviderResponse, error) {
			return model.ExtractProviderResponse{Results: []model.ExtractResult{{URL: targetURL, Content: "preferred", Title: "Exa title"}}, Usage: []model.UsageMeasurement{{Unit: "credits", Quantity: 1}}}, nil
		}},
		extractTestProvider{name: model.ProviderTavily, extract: func(req model.ExtractRequest) (model.ExtractProviderResponse, error) {
			return model.ExtractProviderResponse{Results: []model.ExtractResult{{URL: targetURL, Content: "secondary", Images: []string{"https://example.com/image.png"}}}, Usage: []model.UsageMeasurement{{Unit: "credits", Quantity: 2}}}, nil
		}},
	)
	store := &orchestratorTestStore{
		settings: model.RuntimeSettings{RequestTimeoutMS: 1000, AllowPrivateExtractTargets: true},
		providers: []model.ProviderConfig{
			{Name: model.ProviderExa, Enabled: true, Priority: 1, Weight: 1},
			{Name: model.ProviderTavily, Enabled: true, Priority: 2, Weight: 1},
		},
	}
	orchestrator := NewOrchestrator(registry, &orchestratorTestKeyPool{}, store)

	response, err := orchestrator.Extract(context.Background(), model.ExtractRequest{
		URLs:              []string{targetURL},
		Providers:         []string{model.ProviderExa, model.ProviderTavily},
		ProvidersExplicit: true,
		Mode:              model.SearchModeParallel,
		IncludeImages:     true,
	}, "extract-2", 0)
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("Results = %+v, want one merged result", response.Results)
	}
	result := response.Results[0]
	if result.Content != "preferred" {
		t.Fatalf("Content = %q, want first provider content", result.Content)
	}
	if got, want := result.Providers, []string{model.ProviderExa, model.ProviderTavily}; !stringSlicesEqual(got, want) {
		t.Fatalf("Providers = %v, want %v", got, want)
	}
	if len(result.Images) != 1 {
		t.Fatalf("Images = %v, want enrichment from second provider", result.Images)
	}
	if len(response.Usage) != 2 || response.Usage[0].Quantity+response.Usage[1].Quantity != 3 {
		t.Fatalf("response usage = %+v, want both providers", response.Usage)
	}
}

func TestExtractReportsOneAggregatedFailurePerURL(t *testing.T) {
	targetURL := "https://example.com/unavailable"
	registry := provider.NewRegistry(
		extractTestProvider{name: model.ProviderExa, extract: func(req model.ExtractRequest) (model.ExtractProviderResponse, error) {
			return model.ExtractProviderResponse{}, &provider.Error{Type: provider.ErrorTypeAuth, Message: "invalid key"}
		}},
		extractTestProvider{name: model.ProviderJina, extract: func(req model.ExtractRequest) (model.ExtractProviderResponse, error) {
			return model.ExtractProviderResponse{FailedResults: []model.ExtractFailure{{URL: targetURL, ErrorType: provider.ErrorTypeUpstream, Error: "reader failed"}}}, nil
		}},
	)
	store := &orchestratorTestStore{
		settings: model.RuntimeSettings{RequestTimeoutMS: 1000, AllowPrivateExtractTargets: true},
		providers: []model.ProviderConfig{
			{Name: model.ProviderExa, Enabled: true, Priority: 1, Weight: 1, Settings: map[string]interface{}{"key_retry_count": 0}},
			{Name: model.ProviderJina, Enabled: true, Priority: 2, Weight: 1},
		},
	}
	orchestrator := NewOrchestrator(registry, &orchestratorTestKeyPool{}, store)

	response, err := orchestrator.Extract(context.Background(), model.ExtractRequest{
		URLs:              []string{targetURL},
		Providers:         []string{model.ProviderExa, model.ProviderJina},
		ProvidersExplicit: true,
	}, "extract-3", 0)
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if len(response.Results) != 0 || len(response.FailedResults) != 1 {
		t.Fatalf("response = %+v, want one failed URL", response)
	}
	if !stringsContainAll(response.FailedResults[0].Error, "exa: auth: invalid key", "jina: reader failed") {
		t.Fatalf("failure = %q, want errors from both providers", response.FailedResults[0].Error)
	}
}

func TestExtractReturnsErrorWhenEveryProviderFailsSystemically(t *testing.T) {
	targetURL := "https://example.com/unavailable"
	registry := provider.NewRegistry(
		extractTestProvider{name: model.ProviderExa, extract: func(req model.ExtractRequest) (model.ExtractProviderResponse, error) {
			return model.ExtractProviderResponse{}, &provider.Error{Type: provider.ErrorTypeAuth, Message: "invalid key"}
		}},
		extractTestProvider{name: model.ProviderTavily, extract: func(req model.ExtractRequest) (model.ExtractProviderResponse, error) {
			return model.ExtractProviderResponse{}, &provider.Error{Type: provider.ErrorTypeRateLimited, Message: "limited"}
		}},
	)
	store := &extractTestStore{orchestratorTestStore: orchestratorTestStore{
		settings: model.RuntimeSettings{RequestTimeoutMS: 1000, AllowPrivateExtractTargets: true},
		providers: []model.ProviderConfig{
			{Name: model.ProviderExa, Enabled: true, Priority: 1, Settings: map[string]interface{}{"key_retry_count": 0}},
			{Name: model.ProviderTavily, Enabled: true, Priority: 2, Settings: map[string]interface{}{"key_retry_count": 0}},
		},
	}}
	orchestrator := NewOrchestrator(registry, &orchestratorTestKeyPool{}, store)

	response, err := orchestrator.Extract(context.Background(), model.ExtractRequest{
		URLs: []string{targetURL}, Providers: []string{model.ProviderExa, model.ProviderTavily}, ProvidersExplicit: true,
	}, "extract-systemic", 0)
	if provider.ErrorType(err) != provider.ErrorTypeAuth {
		t.Fatalf("error = %v, want first systemic auth error", err)
	}
	if len(response.Results) != 0 || len(response.FailedResults) != 1 {
		t.Fatalf("response = %+v, want logged failed URL details", response)
	}
	if len(store.logs) != 1 || store.logs[0].Status != "error" {
		t.Fatalf("logs = %+v, want one error log", store.logs)
	}
}

func TestAdapterForProviderPassesExtractBaseURL(t *testing.T) {
	var captured provider.Config
	registry := provider.NewRegistry()
	registry.RegisterFactory(model.ProviderJina, func(cfg provider.Config) provider.Provider {
		captured = cfg
		return extractTestProvider{name: model.ProviderJina}
	})
	orchestrator := &Orchestrator{registry: registry}
	_, ok := orchestrator.adapterForProvider(model.ProviderJina, model.ProviderConfig{
		Name:    model.ProviderJina,
		BaseURL: "https://search.example.test",
		Settings: map[string]interface{}{
			"extract_base_url": " https://reader.example.test/ ",
		},
	}, 1200, "")
	if !ok {
		t.Fatal("adapterForProvider did not build registered factory")
	}
	if got, want := captured.ExtractBaseURL, "https://reader.example.test/"; got != want {
		t.Fatalf("ExtractBaseURL = %q, want %q", got, want)
	}
}

func TestExtractResponseLogPayloadKeepsBoundedPreview(t *testing.T) {
	content := strings.Repeat("x", maxExtractLogContentBytes+10)
	result := model.ExtractResult{
		URL:     "https://example.com/large",
		Content: content,
		Raw:     map[string]interface{}{"body": content},
	}
	payload := extractResponseLogPayload(
		model.ExtractResponse{Results: []model.ExtractResult{result}},
		[]extractProviderExecution{{provider: model.ProviderExa, results: []model.ExtractResult{result}}},
	)
	if len(payload.Results) != 1 || len(payload.Results[0].Content) != maxExtractLogContentBytes {
		t.Fatalf("logged content length = %d, want %d", len(payload.Results[0].Content), maxExtractLogContentBytes)
	}
	if !payload.Results[0].LogContentTruncated || !payload.Results[0].RawOmitted || payload.Results[0].Raw != nil {
		t.Fatalf("unexpected merged log result: %+v", payload.Results[0])
	}
	if len(payload.ProviderResults) != 1 || len(payload.ProviderResults[0].Results) != 1 || !payload.ProviderResults[0].Results[0].LogContentTruncated {
		t.Fatalf("unexpected provider log results: %+v", payload.ProviderResults)
	}
}

func TestNormalizeExtractProviderResultsBoundsLargeFields(t *testing.T) {
	targetURL := "https://example.com/large"
	images := make([]string, 0, maxExtractImagesPerResult+10)
	for index := 0; index < maxExtractImagesPerResult+10; index++ {
		images = append(images, "https://example.com/image/"+strings.Repeat("x", 1024)+string(rune('a'+index%26)))
	}
	results, failures := normalizeExtractProviderResults([]model.ExtractResult{{
		URL:     targetURL,
		Title:   strings.Repeat("t", maxExtractTitleBytes+10),
		Content: strings.Repeat("c", maxExtractResultContentBytes+10),
		Images:  images,
		Raw:     map[string]interface{}{"body": strings.Repeat("r", maxExtractRawBytes+10)},
	}}, model.ProviderExa, model.ExtractRequest{
		URLs:          []string{targetURL},
		Format:        model.ExtractFormatMarkdown,
		IncludeImages: true,
		IncludeRaw:    true,
	})
	if len(failures) != 0 || len(results) != 1 {
		t.Fatalf("unexpected normalization result: results=%d failures=%+v", len(results), failures)
	}
	result := results[0]
	if len(result.Content) != maxExtractResultContentBytes || !result.ContentTruncated {
		t.Fatalf("content length/truncation = %d/%v", len(result.Content), result.ContentTruncated)
	}
	if len(result.Title) != maxExtractTitleBytes {
		t.Fatalf("title length = %d, want %d", len(result.Title), maxExtractTitleBytes)
	}
	if !result.ImagesTruncated || len(result.Images) > maxExtractImagesPerResult {
		t.Fatalf("images were not bounded: count=%d truncated=%v", len(result.Images), result.ImagesTruncated)
	}
	if !result.RawTruncated || result.Raw["_truncated"] != true {
		t.Fatalf("raw payload was not replaced with a truncation marker: %#v", result.Raw)
	}
}

func TestBoundExtractResponseHonorsAggregateAndSerializedBudgets(t *testing.T) {
	results := make([]model.ExtractResult, 0, maxExtractURLs)
	for index := 0; index < maxExtractURLs; index++ {
		result := model.ExtractResult{
			URL:      "https://example.com/article",
			Content:  strings.Repeat("\x01", maxExtractResultContentBytes),
			Provider: model.ProviderExa,
			Raw:      map[string]interface{}{"metadata": strings.Repeat("r", maxExtractRawBytes/2)},
			Images:   []string{"https://example.com/image.png"},
		}
		boundExtractResult(&result, true)
		results = append(results, result)
	}
	boundExtractMergedResults(results)
	totalContent := 0
	for _, result := range results {
		totalContent += len(result.Content)
	}
	if totalContent > maxExtractResponseContentBytes {
		t.Fatalf("aggregate content bytes = %d, want <= %d", totalContent, maxExtractResponseContentBytes)
	}

	response := model.ExtractResponse{Results: results}
	boundExtractResponse(&response)
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal bounded response: %v", err)
	}
	if len(payload) > maxExtractResponseJSONBytes {
		t.Fatalf("serialized response bytes = %d, want <= %d", len(payload), maxExtractResponseJSONBytes)
	}
	if !response.Meta.ResponseTruncated {
		t.Fatal("response_truncated should report aggregate size enforcement")
	}
	for index, result := range response.Results {
		if result.Content == "" || !result.ContentTruncated {
			t.Fatalf("result %d lost its bounded content marker: %+v", index, result)
		}
	}
}

func TestMarshalExtractResponseLogHonorsHardBudget(t *testing.T) {
	content := strings.Repeat("\x01", maxExtractLogContentBytes+100)
	image := strings.Repeat("\x02", maxExtractLogImageBytes)
	results := make([]model.ExtractResult, 0, maxExtractURLs)
	for index := 0; index < maxExtractURLs; index++ {
		results = append(results, model.ExtractResult{
			URL:      "https://example.com/article",
			Content:  content,
			Images:   []string{image},
			Provider: model.ProviderExa,
		})
	}
	executions := make([]extractProviderExecution, 0, len(model.ExtractProviders))
	for _, providerName := range model.ExtractProviders {
		executions = append(executions, extractProviderExecution{provider: providerName, status: "success", results: results})
	}

	payload := marshalExtractResponseLog(model.ExtractResponse{Results: results}, executions)
	if len(payload) > maxExtractLogJSONBytes {
		t.Fatalf("log payload bytes = %d, want <= %d", len(payload), maxExtractLogJSONBytes)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode bounded log payload: %v", err)
	}
	if decoded["truncated"] != true {
		t.Fatalf("bounded log payload should report truncation: %#v", decoded)
	}
}

type extractTestStore struct {
	orchestratorTestStore
	mu   sync.Mutex
	logs []model.SearchLogInput
}

func (s *extractTestStore) RecordSearchLog(ctx context.Context, input model.SearchLogInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, input)
	return nil
}

func nestedStringSlicesEqual(left, right [][]string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !stringSlicesEqual(left[index], right[index]) {
			return false
		}
	}
	return true
}

func stringsContainAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}
