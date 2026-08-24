package search

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/one-search/one-search/backend/internal/model"
	"github.com/one-search/one-search/backend/internal/provider"
)

const (
	maxExtractURLs                 = 20
	maxExtractURLBytes             = 8192
	maxExtractResultContentBytes   = 512 * 1024
	maxExtractResponseContentBytes = 8 * 1024 * 1024
	maxExtractResponseJSONBytes    = 12 * 1024 * 1024
	maxExtractRawBytes             = 64 * 1024
	maxExtractImagesPerResult      = 64
	maxExtractImageBytesPerResult  = 64 * 1024
	maxExtractTitleBytes           = 4096
	maxExtractAuthorBytes          = 1024
	maxExtractErrorBytes           = 4096
	maxExtractProviderItems        = 100
	maxExtractLogContentBytes      = 4096
	maxExtractLogImageBytes        = 4096
	maxExtractLogErrorBytes        = 1024
	maxExtractLogJSONBytes         = 4 * 1024 * 1024
	extractDNSCheckTimeout         = 3 * time.Second
	usageLogTimeout                = 2 * time.Second
)

var blockedExtractPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("100:0:0:1::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:10::/28"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

// ExtractValidationError identifies client-side request errors so HTTP and MCP
// callers can map them to their respective invalid-argument responses.
type ExtractValidationError struct {
	Message string
}

func (e *ExtractValidationError) Error() string {
	if e == nil {
		return "invalid extract request"
	}
	return e.Message
}

// Extract fetches content for already-known URLs. Unlike Search, extraction is
// deliberately not cached: the upstream page is expected to be the source of
// truth for every request.
func (o *Orchestrator) Extract(ctx context.Context, req model.ExtractRequest, requestID string, apiTokenID int64) (model.ExtractResponse, error) {
	started := time.Now()
	ctx, compatCancel := withExtractCompatibilityTimeout(ctx, req)
	defer compatCancel()

	settings, err := o.store.RuntimeSettings(ctx)
	if err != nil {
		return model.ExtractResponse{}, err
	}
	providerConfigs, err := o.store.ListProviders(ctx)
	if err != nil {
		return model.ExtractResponse{}, err
	}
	req, err = applyExtractDefaults(req, settings)
	if err != nil {
		return model.ExtractResponse{}, err
	}
	if !settings.AllowPrivateExtractTargets {
		if err := o.validateResolvedExtractTargets(ctx, req.URLs); err != nil {
			return model.ExtractResponse{}, err
		}
	}
	if !req.ProvidersExplicit {
		req.Providers = routeProviders(req.Providers, providerConfigs, settings.ProviderRoutingStrategy)
	}
	req.Providers = filterEnabledProviders(req.Providers, providerConfigs)

	providerConfigByName := providerConfigMap(providerConfigs)
	providerSettings := providerSettingsFromProviders(providerConfigs)
	keyRetryCounts := providerKeyRetryCounts(providerSettings)
	providerTimeouts := providerTimeouts(providerSettings)
	providerTimeouts = adjustedExtractProviderTimeouts(req, providerTimeouts)
	providerProxies := providerProxies(providerSettings)
	retryableErrors := providerRetryableErrors(providerSettings)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(effectiveExtractRequestTimeoutMS(settings.RequestTimeoutMS, req, providerTimeouts))*time.Millisecond)
	defer cancel()

	executions := o.executeExtract(ctx, req, providerConfigByName, keyRetryCounts, providerTimeouts, providerProxies, retryableErrors)
	results, failedResults := mergeExtractResults(executions, req.URLs)
	boundExtractMergedResults(results)
	status := "success"
	errorMessage := ""
	if len(results) == 0 {
		status = "error"
		errorMessage = firstExtractError(executions, failedResults)
	}
	response := model.ExtractResponse{
		Results:       results,
		FailedResults: failedResults,
		Providers:     extractSummaries(executions),
		Usage:         extractUsageMeasurements(executions),
		Meta: model.ExtractMetadata{
			RequestID:        requestID,
			Mode:             req.Mode,
			CompatFormat:     req.CompatFormat,
			LatencyMS:        time.Since(started).Milliseconds(),
			TotalResults:     len(results),
			TotalFailed:      len(failedResults),
			ProvidersQueried: extractProvidersQueried(executions),
		},
	}
	boundExtractResponse(&response)

	requestJSON, _ := json.Marshal(req)
	responseJSON := marshalExtractResponseLog(response, executions)
	logErr := o.recordSearchLog(model.SearchLogInput{
		Operation:    "extract",
		RequestID:    requestID,
		APITokenID:   apiTokenID,
		Query:        strings.Join(req.URLs, "\n"),
		Mode:         string(req.Mode),
		CompatFormat: string(req.CompatFormat),
		Providers:    req.Providers,
		CachePolicy:  string(model.CachePolicyBypass),
		CacheHit:     false,
		ResultCount:  len(response.Results),
		Status:       status,
		ErrorMessage: errorMessage,
		LatencyMS:    response.Meta.LatencyMS,
		RequestJSON:  requestJSON,
		ResponseJSON: responseJSON,
		Calls:        extractCallLogs(executions),
	})
	if len(response.Results) == 0 {
		if err := ctx.Err(); err != nil {
			return response, err
		}
		if err := allProvidersSystemicExtractError(executions); err != nil {
			return response, err
		}
	}
	if logErr != nil {
		return response, fmt.Errorf("record extract usage: %w", logErr)
	}
	return response, nil
}

func allProvidersSystemicExtractError(executions []extractProviderExecution) error {
	if len(executions) == 0 {
		return &provider.Error{Type: provider.ErrorTypeNoKey, Message: "no enabled extract provider is available"}
	}
	for _, execution := range executions {
		if execution.err == nil {
			return nil
		}
	}
	return executions[0].err
}

func withExtractCompatibilityTimeout(ctx context.Context, req model.ExtractRequest) (context.Context, context.CancelFunc) {
	if req.CompatFormat == model.CompatFormatTavily {
		if seconds, provided, err := extractNumericOption(req.Options, "timeout"); err == nil && provided && seconds > 0 {
			return context.WithTimeout(ctx, time.Duration(int64(math.Ceil(seconds*1000))+2000)*time.Millisecond)
		}
	}
	return ctx, func() {}
}

func applyExtractDefaults(req model.ExtractRequest, settings model.RuntimeSettings) (model.ExtractRequest, error) {
	if len(req.URLs) == 0 {
		return model.ExtractRequest{}, &ExtractValidationError{Message: "urls must contain at least one URL"}
	}
	if len(req.URLs) > maxExtractURLs {
		return model.ExtractRequest{}, &ExtractValidationError{Message: fmt.Sprintf("urls must contain at most %d URLs", maxExtractURLs)}
	}

	seenURLs := map[string]bool{}
	urls := make([]string, 0, len(req.URLs))
	for index, rawURL := range req.URLs {
		value := strings.TrimSpace(rawURL)
		if len(value) > maxExtractURLBytes {
			return model.ExtractRequest{}, &ExtractValidationError{Message: fmt.Sprintf("urls[%d] must be at most %d bytes", index, maxExtractURLBytes)}
		}
		parsed, err := url.ParseRequestURI(value)
		httpScheme := parsed != nil && (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https"))
		if err != nil || parsed == nil || parsed.Host == "" || !httpScheme || parsed.User != nil {
			return model.ExtractRequest{}, &ExtractValidationError{Message: fmt.Sprintf("urls[%d] must be an absolute http or https URL without credentials", index)}
		}
		if !settings.AllowPrivateExtractTargets && blockedExtractHostname(parsed.Hostname()) {
			return model.ExtractRequest{}, &ExtractValidationError{Message: fmt.Sprintf("urls[%d] targets a private or reserved network; enable allow_private_extract_targets only for trusted clients", index)}
		}
		key := extractURLKey(value)
		if !seenURLs[key] {
			seenURLs[key] = true
			urls = append(urls, value)
		}
	}
	req.URLs = urls
	req.Query = strings.TrimSpace(req.Query)

	switch req.Mode {
	case "":
		req.Mode = model.SearchModeFallback
	case model.SearchModeFallback, model.SearchModeParallel, model.SearchModeSingle:
	default:
		return model.ExtractRequest{}, &ExtractValidationError{Message: "mode must be fallback, parallel, or single"}
	}
	switch req.Format {
	case "":
		req.Format = model.ExtractFormatMarkdown
	case model.ExtractFormatMarkdown, model.ExtractFormatText, model.ExtractFormatHTML, model.ExtractFormatRawHTML:
	default:
		return model.ExtractRequest{}, &ExtractValidationError{Message: "format must be markdown, text, html, or raw_html"}
	}
	switch req.ExtractDepth {
	case "", "basic", "advanced":
	default:
		return model.ExtractRequest{}, &ExtractValidationError{Message: "extract_depth must be basic or advanced"}
	}
	chunksProvided := req.ChunksPerSourceSet || req.ChunksPerSource != 0
	if chunksProvided && (req.ChunksPerSource < 1 || req.ChunksPerSource > 5) {
		return model.ExtractRequest{}, &ExtractValidationError{Message: "chunks_per_source must be between 1 and 5 when provided"}
	}
	if chunksProvided && req.Query == "" {
		return model.ExtractRequest{}, &ExtractValidationError{Message: "query is required when chunks_per_source is provided"}
	}
	if req.CompatFormat == "" {
		req.CompatFormat = model.CompatFormatNative
	}
	if req.CompatFormat == model.CompatFormatTavily {
		if req.Format != model.ExtractFormatMarkdown && req.Format != model.ExtractFormatText {
			return model.ExtractRequest{}, &ExtractValidationError{Message: "format must be markdown or text for Tavily compatibility"}
		}
		timeout, provided, err := extractNumericOption(req.Options, "timeout")
		if err != nil {
			return model.ExtractRequest{}, &ExtractValidationError{Message: "timeout must be a number between 1 and 60 seconds"}
		}
		if provided && (timeout < 1 || timeout > 60) {
			return model.ExtractRequest{}, &ExtractValidationError{Message: "timeout must be between 1 and 60 seconds when provided"}
		}
	}

	providersConstrained := len(req.Providers) > 0
	if !providersConstrained {
		if len(settings.DefaultProviders) > 0 {
			req.Providers = extractCapableProviders(settings.DefaultProviders)
		} else {
			req.Providers = append([]string(nil), model.ExtractProviders...)
		}
	}
	if providersConstrained && !req.ProvidersExplicit {
		req.Providers = extractCapableProviders(req.Providers)
	}
	providers := make([]string, 0, len(req.Providers))
	seenProviders := map[string]bool{}
	for _, rawName := range req.Providers {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if !isExtractProvider(name) {
			return model.ExtractRequest{}, &ExtractValidationError{Message: fmt.Sprintf("provider %q does not support extract", rawName)}
		}
		if !seenProviders[name] {
			seenProviders[name] = true
			providers = append(providers, name)
		}
	}
	req.Providers = providers
	return req, nil
}

func (o *Orchestrator) validateResolvedExtractTargets(ctx context.Context, urls []string) error {
	type target struct {
		hostname string
		index    int
	}
	targets := make([]target, 0, len(urls))
	seen := map[string]bool{}
	for index, rawURL := range urls {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return &ExtractValidationError{Message: fmt.Sprintf("urls[%d] could not be parsed for network validation", index)}
		}
		hostname := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
		if net.ParseIP(hostname) != nil || seen[hostname] {
			continue
		}
		seen[hostname] = true
		targets = append(targets, target{hostname: hostname, index: index})
	}
	if len(targets) == 0 {
		return nil
	}

	lookup := o.lookupIPAddr
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	lookupCtx, cancel := context.WithTimeout(ctx, extractDNSCheckTimeout)
	defer cancel()
	type lookupResult struct {
		target    target
		addresses []net.IPAddr
		err       error
	}
	results := make(chan lookupResult, len(targets))
	for _, item := range targets {
		go func(current target) {
			addresses, err := lookup(lookupCtx, current.hostname)
			results <- lookupResult{target: current, addresses: addresses, err: err}
		}(item)
	}
	for range targets {
		var result lookupResult
		select {
		case result = <-results:
		case <-lookupCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &ExtractValidationError{Message: "hostname resolution timed out during extract target validation"}
		}
		if result.err != nil || len(result.addresses) == 0 {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &ExtractValidationError{Message: fmt.Sprintf("urls[%d] hostname could not be resolved safely", result.target.index)}
		}
		for _, address := range result.addresses {
			if blockedExtractIP(address.IP) {
				return &ExtractValidationError{Message: fmt.Sprintf("urls[%d] resolves to a private or reserved network; enable allow_private_extract_targets only for trusted clients", result.target.index)}
			}
		}
	}
	return nil
}

func blockedExtractHostname(rawHostname string) bool {
	hostname := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(rawHostname), "."))
	if hostname == "" || strings.Contains(hostname, "%") {
		return true
	}
	for _, suffix := range []string{"localhost", "local", "internal", "lan", "home", "home.arpa", "svc", "consul"} {
		if hostname == suffix || strings.HasSuffix(hostname, "."+suffix) {
			return true
		}
	}
	ip := net.ParseIP(hostname)
	if ip == nil {
		return looksLikeObscureIPAddress(hostname)
	}
	return blockedExtractIP(ip)
}

func blockedExtractIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	address = address.Unmap()
	for _, prefix := range blockedExtractPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func adjustedExtractProviderTimeouts(req model.ExtractRequest, configured map[string]int) map[string]int {
	timeouts := make(map[string]int, len(configured))
	for name, timeout := range configured {
		timeouts[name] = timeout
	}
	if req.CompatFormat != model.CompatFormatTavily {
		return timeouts
	}
	seconds, provided, err := extractNumericOption(req.Options, "timeout")
	if err != nil || !provided || seconds <= 0 {
		return timeouts
	}
	requested := int(math.Ceil(seconds*1000)) + 1000
	for _, providerName := range req.Providers {
		if requested > timeouts[providerName] {
			timeouts[providerName] = requested
		}
	}
	return timeouts
}

func effectiveExtractRequestTimeoutMS(runtimeTimeoutMS int, req model.ExtractRequest, timeouts map[string]int) int {
	// Tavily's compatibility timeout is a caller-visible deadline for the
	// entire Extract operation. It must still bound aggregate fallback routes,
	// even when the selected upstream is Exa, Jina, or Firecrawl rather than
	// Tavily itself. Keep a small transport envelope around the requested
	// upstream wait time.
	if req.CompatFormat == model.CompatFormatTavily {
		if seconds, provided, err := extractNumericOption(req.Options, "timeout"); err == nil && provided && seconds > 0 {
			return int(math.Ceil(seconds*1000)) + 2000
		}
	}

	baseline := runtimeTimeoutMS
	if baseline <= 0 {
		baseline = 20000
	}
	providerBudget := func(name string) int64 {
		perCall := timeouts[name]
		if perCall <= 0 {
			perCall = 15000
		}
		waves := extractProviderWaves(name, len(req.URLs))
		return int64(perCall)*int64(waves) + 1000
	}

	var required int64
	switch req.Mode {
	case model.SearchModeSingle:
		if len(req.Providers) > 0 {
			required = providerBudget(req.Providers[0])
		}
	case model.SearchModeParallel:
		for _, name := range req.Providers {
			if budget := providerBudget(name); budget > required {
				required = budget
			}
		}
	default:
		for _, name := range req.Providers {
			required += providerBudget(name)
		}
	}
	if required < int64(baseline) {
		return baseline
	}
	return int(required)
}

func extractProviderWaves(providerName string, urlCount int) int {
	if urlCount <= 1 || (providerName != model.ProviderJina && providerName != model.ProviderFirecrawl) {
		return 1
	}
	return urlCount
}

func looksLikeObscureIPAddress(hostname string) bool {
	if strings.HasPrefix(hostname, "0x") {
		return true
	}
	for _, character := range hostname {
		if (character < '0' || character > '9') && character != '.' {
			return false
		}
	}
	return true
}

func extractNumericOption(options map[string]interface{}, key string) (float64, bool, error) {
	if options == nil {
		return 0, false, nil
	}
	value, ok := options[key]
	if !ok {
		return 0, false, nil
	}
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case float32:
		number = float64(typed)
	case int:
		number = float64(typed)
	case int32:
		number = float64(typed)
	case int64:
		number = float64(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, true, err
		}
		number = parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, true, err
		}
		number = parsed
	default:
		return 0, true, fmt.Errorf("option %s must be numeric", key)
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, true, fmt.Errorf("option %s must be finite", key)
	}
	return number, true, nil
}

func extractCapableProviders(names []string) []string {
	items := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, rawName := range names {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if isExtractProvider(name) && !seen[name] {
			seen[name] = true
			items = append(items, name)
		}
	}
	return items
}

func isExtractProvider(name string) bool {
	for _, supported := range model.ExtractProviders {
		if name == supported {
			return true
		}
	}
	return false
}

func (o *Orchestrator) executeExtract(
	ctx context.Context,
	req model.ExtractRequest,
	providerConfigs map[string]model.ProviderConfig,
	keyRetryCounts map[string]int,
	providerTimeouts map[string]int,
	providerProxies map[string]string,
	retryableErrors map[string]map[string]bool,
) []extractProviderExecution {
	switch req.Mode {
	case model.SearchModeParallel:
		return o.extractParallel(ctx, req, providerConfigs, keyRetryCounts, providerTimeouts, providerProxies, retryableErrors)
	case model.SearchModeSingle:
		return o.extractSingle(ctx, req, providerConfigs, keyRetryCounts, providerTimeouts, providerProxies, retryableErrors)
	default:
		return o.extractFallback(ctx, req, providerConfigs, keyRetryCounts, providerTimeouts, providerProxies, retryableErrors)
	}
}

func (o *Orchestrator) extractParallel(ctx context.Context, req model.ExtractRequest, providerConfigs map[string]model.ProviderConfig, keyRetryCounts map[string]int, providerTimeouts map[string]int, providerProxies map[string]string, retryableErrors map[string]map[string]bool) []extractProviderExecution {
	var wg sync.WaitGroup
	results := make([]extractProviderExecution, len(req.Providers))
	for index, name := range req.Providers {
		wg.Add(1)
		go func(i int, providerName string) {
			defer wg.Done()
			results[i] = o.callExtractProvider(ctx, req, providerName, providerConfigs, keyRetryCounts, providerTimeouts, providerProxies, retryableErrors)
		}(index, name)
	}
	wg.Wait()
	return results
}

func (o *Orchestrator) extractFallback(ctx context.Context, req model.ExtractRequest, providerConfigs map[string]model.ProviderConfig, keyRetryCounts map[string]int, providerTimeouts map[string]int, providerProxies map[string]string, retryableErrors map[string]map[string]bool) []extractProviderExecution {
	remaining := append([]string(nil), req.URLs...)
	results := make([]extractProviderExecution, 0, len(req.Providers))
	for _, name := range req.Providers {
		providerReq := req
		providerReq.URLs = append([]string(nil), remaining...)
		execution := o.callExtractProvider(ctx, providerReq, name, providerConfigs, keyRetryCounts, providerTimeouts, providerProxies, retryableErrors)
		results = append(results, execution)
		remaining = unresolvedExtractURLs(remaining, execution.results)
		if len(remaining) == 0 {
			break
		}
	}
	return results
}

func (o *Orchestrator) extractSingle(ctx context.Context, req model.ExtractRequest, providerConfigs map[string]model.ProviderConfig, keyRetryCounts map[string]int, providerTimeouts map[string]int, providerProxies map[string]string, retryableErrors map[string]map[string]bool) []extractProviderExecution {
	if len(req.Providers) == 0 {
		return nil
	}
	return []extractProviderExecution{o.callExtractProvider(ctx, req, req.Providers[0], providerConfigs, keyRetryCounts, providerTimeouts, providerProxies, retryableErrors)}
}

func (o *Orchestrator) callExtractProvider(ctx context.Context, req model.ExtractRequest, providerName string, providerConfigs map[string]model.ProviderConfig, keyRetryCounts map[string]int, providerTimeouts map[string]int, providerProxies map[string]string, retryableErrors map[string]map[string]bool) extractProviderExecution {
	started := time.Now()
	execution := extractProviderExecution{provider: providerName, status: "error"}
	adapter, ok := o.adapterForProvider(providerName, providerConfigs[providerName], providerTimeouts[providerName], providerProxies[providerName])
	if !ok {
		return failedExtractExecution(execution, started, &provider.Error{Type: provider.ErrorTypeUpstream, Message: "provider is not registered"})
	}
	extractor, ok := adapter.(provider.Extractor)
	if !ok {
		return failedExtractExecution(execution, started, &provider.Error{Type: provider.ErrorTypeUpstream, Message: "provider does not support extract"})
	}
	batchSize := len(req.URLs)
	if sizer, ok := adapter.(provider.ExtractBatchSizer); ok && sizer.ExtractBatchSize() > 0 {
		batchSize = sizer.ExtractBatchSize()
	}
	if batchSize <= 0 || len(req.URLs) <= batchSize {
		return o.callExtractAdapter(ctx, req, providerName, extractor, keyRetryCounts, providerTimeouts, retryableErrors)
	}

	// Providers such as Jina Reader and Firecrawl perform one upstream HTTP
	// request per URL. Execute those requests sequentially and meter each one
	// independently so RPM, key rotation, retries and billing match the real
	// number of upstream calls. Stop the remaining batches after a systemic
	// provider error while preserving results already collected.
	batches := make([]model.ExtractRequest, 0, (len(req.URLs)+batchSize-1)/batchSize)
	for offset := 0; offset < len(req.URLs); offset += batchSize {
		end := offset + batchSize
		if end > len(req.URLs) {
			end = len(req.URLs)
		}
		batchReq := req
		batchReq.URLs = append([]string(nil), req.URLs[offset:end]...)
		batches = append(batches, batchReq)
	}
	aggregated := extractProviderExecution{provider: providerName, status: "success"}
	appendBatch := func(batch extractProviderExecution) {
		aggregated.results = append(aggregated.results, batch.results...)
		aggregated.failedResults = append(aggregated.failedResults, batch.failedResults...)
		aggregated.key = batch.key
		aggregated.keyAlias = batch.keyAlias
		for _, attempt := range batch.attempts {
			attempt.AttemptIndex = len(aggregated.attempts) + 1
			aggregated.attempts = append(aggregated.attempts, attempt)
		}
		if batch.err != nil && aggregated.err == nil {
			aggregated.status = "error"
			aggregated.err = batch.err
			aggregated.errorType = batch.errorType
		}
	}

	for _, batchReq := range batches {
		batch := o.callExtractAdapter(ctx, batchReq, providerName, extractor, keyRetryCounts, providerTimeouts, retryableErrors)
		appendBatch(batch)
		if batch.err != nil {
			break
		}
	}
	aggregated.latencyMS = time.Since(started).Milliseconds()
	return aggregated
}

func (o *Orchestrator) callExtractAdapter(ctx context.Context, req model.ExtractRequest, providerName string, extractor provider.Extractor, keyRetryCounts map[string]int, providerTimeouts map[string]int, retryableErrors map[string]map[string]bool) extractProviderExecution {
	started := time.Now()
	execution := extractProviderExecution{provider: providerName, status: "error"}

	retryCount, ok := keyRetryCounts[providerName]
	if !ok {
		retryCount = 3
	}
	if retryCount < 0 {
		retryCount = 0
	}
	if retryCount > 20 {
		retryCount = 20
	}
	for attempt := 0; attempt <= retryCount; attempt++ {
		attemptStarted := time.Now()
		attemptIndex := attempt + 1
		key, release, err := o.keyPool.Acquire(ctx, providerName)
		if err != nil {
			execution.err = err
			execution.errorType = provider.ErrorType(err)
			execution.latencyMS = time.Since(started).Milliseconds()
			execution.attempts = append(execution.attempts, providerAttempt{AttemptIndex: attemptIndex, Status: "error", ErrorType: execution.errorType, Err: err, LatencyMS: time.Since(attemptStarted).Milliseconds()})
			return execution
		}
		callCtx := ctx
		cancel := func() {}
		if timeout := providerTimeouts[providerName]; timeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
		}
		providerResponse, err := extractor.Extract(callCtx, req, key)
		cancel()
		success := err == nil
		release(success, err)
		o.refreshOfficialQuota(key)

		attemptLatency := time.Since(attemptStarted).Milliseconds()
		execution.latencyMS = time.Since(started).Milliseconds()
		execution.key = key
		execution.keyAlias = key.Alias
		if err == nil {
			execution.status = "success"
			execution.err = nil
			execution.errorType = ""
			execution.results, execution.failedResults = normalizeExtractProviderResults(providerResponse.Results, providerName, req)
			execution.failedResults = append(execution.failedResults, normalizeExtractProviderFailures(providerResponse.FailedResults, providerName)...)
			execution.attempts = append(execution.attempts, providerAttempt{Key: key, KeyAlias: key.Alias, AttemptIndex: attemptIndex, Status: "success", LatencyMS: attemptLatency, ResultCount: len(execution.results), Usage: providerResponse.Usage})
			return execution
		}

		willRetry := attempt < retryCount && shouldRetryWithNextKey(err, retryableErrors[providerName])
		execution.err = err
		execution.errorType = provider.ErrorType(err)
		execution.attempts = append(execution.attempts, providerAttempt{Key: key, KeyAlias: key.Alias, AttemptIndex: attemptIndex, WillRetry: willRetry, Status: "error", ErrorType: execution.errorType, Err: err, LatencyMS: attemptLatency})
		if !willRetry {
			return execution
		}
	}
	return execution
}

func failedExtractExecution(execution extractProviderExecution, started time.Time, err error) extractProviderExecution {
	execution.err = err
	execution.errorType = provider.ErrorType(err)
	execution.latencyMS = time.Since(started).Milliseconds()
	execution.attempts = append(execution.attempts, providerAttempt{AttemptIndex: 1, Status: "error", ErrorType: execution.errorType, Err: err, LatencyMS: execution.latencyMS})
	return execution
}

func normalizeExtractProviderResults(results []model.ExtractResult, providerName string, req model.ExtractRequest) ([]model.ExtractResult, []model.ExtractFailure) {
	capacity := len(results)
	if capacity > maxExtractProviderItems {
		capacity = maxExtractProviderItems
	}
	items := make([]model.ExtractResult, 0, capacity)
	failures := make([]model.ExtractFailure, 0)
	for index, result := range results {
		if index >= maxExtractProviderItems {
			break
		}
		result.URL = strings.TrimSpace(result.URL)
		if result.URL == "" {
			continue
		}
		requestedURL, matched := correlateExtractResultURL(result.URL, req.URLs)
		if !matched {
			failure := model.ExtractFailure{
				URL:       result.URL,
				Provider:  providerName,
				ErrorType: provider.ErrorTypeInvalidResponse,
				Error:     "provider returned content for an unrequested URL",
			}
			boundExtractFailure(&failure)
			failures = append(failures, failure)
			continue
		}
		result.URL = requestedURL
		if strings.TrimSpace(result.Content) == "" {
			failure := model.ExtractFailure{
				URL:       result.URL,
				Provider:  providerName,
				ErrorType: provider.ErrorTypeInvalidResponse,
				Error:     "provider returned no extracted content",
			}
			boundExtractFailure(&failure)
			failures = append(failures, failure)
			continue
		}
		if result.Provider == "" {
			result.Provider = providerName
		}
		if len(result.Providers) == 0 {
			result.Providers = []string{result.Provider}
		}
		if result.Format == "" {
			result.Format = req.Format
		}
		if !req.IncludeImages {
			result.Images = nil
		}
		if !req.IncludeFavicon {
			result.Favicon = ""
		}
		boundExtractResult(&result, req.IncludeRaw)
		items = append(items, result)
	}
	return items, failures
}

func correlateExtractResultURL(resultURL string, requested []string) (string, bool) {
	resultKey := extractURLKey(resultURL)
	for _, candidate := range requested {
		if extractURLKey(candidate) == resultKey {
			return candidate, true
		}
	}
	// Single-URL adapters and single-item provider requests may legitimately
	// report a redirect/canonical target. There is no ambiguity in that case,
	// so retain the caller's URL as the result correlation key.
	if len(requested) == 1 {
		return requested[0], true
	}
	return "", false
}

func normalizeExtractProviderFailures(failures []model.ExtractFailure, providerName string) []model.ExtractFailure {
	capacity := len(failures)
	if capacity > maxExtractProviderItems {
		capacity = maxExtractProviderItems
	}
	items := make([]model.ExtractFailure, 0, capacity)
	for index, failure := range failures {
		if index >= maxExtractProviderItems {
			break
		}
		failure.URL = strings.TrimSpace(failure.URL)
		if failure.Provider == "" {
			failure.Provider = providerName
		}
		if failure.Error == "" {
			failure.Error = "provider could not extract the URL"
		}
		boundExtractFailure(&failure)
		items = append(items, failure)
	}
	return items
}

func boundExtractResult(result *model.ExtractResult, includeRaw bool) {
	if result == nil {
		return
	}
	result.Title, _ = truncateExtractString(result.Title, maxExtractTitleBytes)
	if content, truncated := truncateExtractString(result.Content, maxExtractResultContentBytes); truncated {
		result.Content = content
		result.ContentTruncated = true
	}
	result.Author, _ = truncateExtractString(result.Author, maxExtractAuthorBytes)
	if len(result.Favicon) > maxExtractURLBytes {
		result.Favicon = ""
		result.ImagesTruncated = true
	}
	result.Images, result.ImagesTruncated = boundExtractImages(
		result.Images,
		maxExtractImagesPerResult,
		maxExtractImageBytesPerResult,
		result.ImagesTruncated,
	)
	if len(result.Providers) > len(model.ExtractProviders) {
		result.Providers = append([]string(nil), result.Providers[:len(model.ExtractProviders)]...)
	}
	if !includeRaw {
		result.Raw = nil
		return
	}
	if raw, truncated := boundExtractRaw(result.Raw); truncated {
		result.Raw = raw
		result.RawTruncated = true
	}
}

func boundExtractFailure(failure *model.ExtractFailure) {
	if failure == nil {
		return
	}
	failure.URL, _ = truncateExtractString(failure.URL, maxExtractURLBytes)
	failure.Provider, _ = truncateExtractString(failure.Provider, 64)
	failure.ErrorType, _ = truncateExtractString(failure.ErrorType, 64)
	failure.Error, _ = truncateExtractString(failure.Error, maxExtractErrorBytes)
}

func boundExtractImages(images []string, maxItems, maxBytes int, alreadyTruncated bool) ([]string, bool) {
	if len(images) == 0 || maxItems <= 0 || maxBytes <= 0 {
		return nil, alreadyTruncated || len(images) > 0
	}
	items := make([]string, 0, minExtractInt(len(images), maxItems))
	used := 0
	seen := map[string]bool{}
	truncated := alreadyTruncated
	for _, raw := range images {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if len(value) > maxExtractURLBytes {
			truncated = true
			continue
		}
		if seen[value] {
			continue
		}
		if len(items) >= maxItems || used+len(value) > maxBytes {
			truncated = true
			continue
		}
		value = strings.Clone(value)
		seen[value] = true
		items = append(items, value)
		used += len(value)
	}
	if len(items) < len(images) {
		truncated = true
	}
	return items, truncated
}

func boundExtractRaw(raw map[string]interface{}) (map[string]interface{}, bool) {
	if raw == nil {
		return nil, false
	}
	payload, err := json.Marshal(raw)
	if err == nil && len(payload) <= maxExtractRawBytes {
		return raw, false
	}
	marker := map[string]interface{}{"_truncated": true}
	if err == nil {
		marker["_original_bytes"] = len(payload)
	}
	return marker, true
}

func truncateExtractString(value string, maxBytes int) (string, bool) {
	if maxBytes < 0 || len(value) <= maxBytes {
		return value, false
	}
	if maxBytes == 0 {
		return "", value != ""
	}
	cut := maxBytes
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return strings.Clone(value[:cut]), true
}

func boundExtractMergedResults(results []model.ExtractResult) {
	if len(results) == 0 {
		return
	}
	total := 0
	maxLength := 0
	for _, result := range results {
		length := len(result.Content)
		total += length
		if length > maxLength {
			maxLength = length
		}
	}
	if total <= maxExtractResponseContentBytes {
		return
	}

	low, high := 0, maxLength
	for low < high {
		candidate := low + (high-low+1)/2
		used := 0
		for _, result := range results {
			used += minExtractInt(len(result.Content), candidate)
			if used > maxExtractResponseContentBytes {
				break
			}
		}
		if used <= maxExtractResponseContentBytes {
			low = candidate
		} else {
			high = candidate - 1
		}
	}
	capExtractResultContent(results, low)
}

func capExtractResultContent(results []model.ExtractResult, maxBytes int) {
	for index := range results {
		if content, truncated := truncateExtractString(results[index].Content, maxBytes); truncated {
			results[index].Content = content
			results[index].ContentTruncated = true
		}
	}
}

func boundExtractResponse(response *model.ExtractResponse) {
	if response == nil {
		return
	}
	for _, result := range response.Results {
		if result.ContentTruncated || result.ImagesTruncated || result.RawTruncated {
			response.Meta.ResponseTruncated = true
			break
		}
	}
	for index := range response.FailedResults {
		boundExtractFailure(&response.FailedResults[index])
	}
	for index := range response.Providers {
		response.Providers[index].Provider, _ = truncateExtractString(response.Providers[index].Provider, 64)
		response.Providers[index].KeyAlias, _ = truncateExtractString(response.Providers[index].KeyAlias, 256)
		response.Providers[index].ErrorType, _ = truncateExtractString(response.Providers[index].ErrorType, 64)
		response.Providers[index].Error, _ = truncateExtractString(response.Providers[index].Error, maxExtractErrorBytes)
	}
	if extractResponseJSONSize(*response) <= maxExtractResponseJSONBytes {
		return
	}
	for index := range response.Results {
		if response.Results[index].Raw != nil {
			response.Results[index].Raw = nil
			response.Results[index].RawTruncated = true
			response.Meta.ResponseTruncated = true
		}
	}
	if extractResponseJSONSize(*response) <= maxExtractResponseJSONBytes {
		return
	}
	for index := range response.Results {
		if len(response.Results[index].Images) > 0 || response.Results[index].Favicon != "" {
			response.Results[index].Images = nil
			response.Results[index].Favicon = ""
			response.Results[index].ImagesTruncated = true
			response.Meta.ResponseTruncated = true
		}
	}
	for contentLimit := maxExtractResultContentBytes / 2; extractResponseJSONSize(*response) > maxExtractResponseJSONBytes; contentLimit /= 2 {
		capExtractResultContent(response.Results, contentLimit)
		response.Meta.ResponseTruncated = true
		if contentLimit == 0 {
			break
		}
	}
	if extractResponseJSONSize(*response) <= maxExtractResponseJSONBytes {
		return
	}

	// The previous bounds make this path practically unreachable, but retain a
	// final compact representation so custom adapters cannot violate the public
	// response-size contract with pathological metadata.
	response.Meta.ResponseTruncated = true
	response.Usage = nil
	for index := range response.Results {
		response.Results[index].Title = ""
		response.Results[index].Content = ""
		response.Results[index].ContentTruncated = true
		response.Results[index].Providers = nil
		response.Results[index].Images = nil
		response.Results[index].ImagesTruncated = true
		response.Results[index].Favicon = ""
		response.Results[index].Author = ""
		response.Results[index].Raw = nil
		response.Results[index].RawTruncated = true
	}
	for index := range response.Providers {
		response.Providers[index].Error = ""
	}
	for index := range response.FailedResults {
		response.FailedResults[index].Error, _ = truncateExtractString(response.FailedResults[index].Error, 512)
	}
}

func extractResponseJSONSize(response model.ExtractResponse) int {
	payload, err := json.Marshal(response)
	if err != nil {
		return maxExtractResponseJSONBytes + 1
	}
	return len(payload)
}

func minExtractInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func unresolvedExtractURLs(requested []string, results []model.ExtractResult) []string {
	succeeded := map[string]bool{}
	for _, result := range results {
		succeeded[extractURLKey(result.URL)] = true
	}
	remaining := make([]string, 0, len(requested))
	for _, rawURL := range requested {
		if !succeeded[extractURLKey(rawURL)] {
			remaining = append(remaining, rawURL)
		}
	}
	return remaining
}

func mergeExtractResults(executions []extractProviderExecution, requested []string) ([]model.ExtractResult, []model.ExtractFailure) {
	requestedKeys := make(map[string]bool, len(requested))
	for _, rawURL := range requested {
		requestedKeys[extractURLKey(rawURL)] = true
	}
	byURL := map[string]int{}
	results := make([]model.ExtractResult, 0, len(requested))
	for _, execution := range executions {
		for _, result := range execution.results {
			key := extractURLKey(result.URL)
			if key == "" || !requestedKeys[key] {
				continue
			}
			if existingIndex, ok := byURL[key]; ok {
				existing := &results[existingIndex]
				for _, providerName := range result.Providers {
					existing.Providers = appendUnique(existing.Providers, providerName)
				}
				existing.Providers = appendUnique(existing.Providers, result.Provider)
				mergeExtractResultDetails(existing, result)
				continue
			}
			if len(result.Providers) == 0 && result.Provider != "" {
				result.Providers = []string{result.Provider}
			}
			byURL[key] = len(results)
			results = append(results, result)
		}
	}

	requestedOrder := map[string]int{}
	for index, rawURL := range requested {
		requestedOrder[extractURLKey(rawURL)] = index
	}
	sort.SliceStable(results, func(i, j int) bool {
		left, leftOK := requestedOrder[extractURLKey(results[i].URL)]
		right, rightOK := requestedOrder[extractURLKey(results[j].URL)]
		if leftOK != rightOK {
			return leftOK
		}
		if leftOK {
			return left < right
		}
		return false
	})

	failed := make([]model.ExtractFailure, 0)
	for _, rawURL := range requested {
		key := extractURLKey(rawURL)
		if _, ok := byURL[key]; ok {
			continue
		}
		failed = append(failed, aggregateExtractFailure(rawURL, key, executions))
	}
	if results == nil {
		results = []model.ExtractResult{}
	}
	return results, failed
}

func mergeExtractResultDetails(target *model.ExtractResult, candidate model.ExtractResult) {
	if target.Title == "" {
		target.Title = candidate.Title
	}
	if target.Content == "" {
		target.Content = candidate.Content
	}
	if target.Format == "" {
		target.Format = candidate.Format
	}
	if len(target.Images) == 0 {
		target.Images = candidate.Images
	}
	if target.Favicon == "" {
		target.Favicon = candidate.Favicon
	}
	if target.Author == "" {
		target.Author = candidate.Author
	}
	if target.PublishedAt == nil {
		target.PublishedAt = candidate.PublishedAt
	}
	if target.Raw == nil {
		target.Raw = candidate.Raw
	}
}

func aggregateExtractFailure(rawURL, key string, executions []extractProviderExecution) model.ExtractFailure {
	messages := make([]string, 0, len(executions))
	errorType := ""
	providers := make([]string, 0, len(executions))
	for _, execution := range executions {
		for _, failure := range execution.failedResults {
			if extractURLKey(failure.URL) != key {
				continue
			}
			providerName := failure.Provider
			if providerName == "" {
				providerName = execution.provider
			}
			providers = appendUnique(providers, providerName)
			messages = append(messages, providerName+": "+failure.Error)
			if errorType == "" {
				errorType = failure.ErrorType
			}
		}
		if execution.err != nil {
			providers = appendUnique(providers, execution.provider)
			messages = append(messages, execution.provider+": "+execution.err.Error())
			if errorType == "" {
				errorType = execution.errorType
			}
		}
	}
	if len(messages) == 0 {
		messages = append(messages, "no provider returned extracted content")
	}
	providerName := ""
	if len(providers) == 1 {
		providerName = providers[0]
	}
	failure := model.ExtractFailure{URL: rawURL, Provider: providerName, ErrorType: errorType, Error: strings.Join(messages, "; ")}
	boundExtractFailure(&failure)
	return failure
}

func extractURLKey(raw string) string {
	value := strings.TrimSpace(raw)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return value
	}
	parsed.Fragment = ""
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	hostname := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		parsed.Host = "[" + hostname + "]"
	} else {
		parsed.Host = hostname
	}
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return parsed.String()
}

func firstExtractError(executions []extractProviderExecution, failures []model.ExtractFailure) string {
	for _, execution := range executions {
		if execution.err != nil {
			message, _ := truncateExtractString(execution.err.Error(), maxExtractErrorBytes)
			return message
		}
	}
	for _, failure := range failures {
		if failure.Error != "" {
			message, _ := truncateExtractString(failure.Error, maxExtractErrorBytes)
			return message
		}
	}
	return "no extract provider available"
}

type extractProviderExecution struct {
	provider      string
	key           model.APIKey
	keyAlias      string
	status        string
	errorType     string
	err           error
	latencyMS     int64
	results       []model.ExtractResult
	failedResults []model.ExtractFailure
	attempts      []providerAttempt
}

func extractSummaries(executions []extractProviderExecution) []model.ProviderCallSummary {
	items := make([]model.ProviderCallSummary, 0, len(executions))
	for _, execution := range executions {
		message := ""
		if execution.err != nil {
			message, _ = truncateExtractString(execution.err.Error(), maxExtractErrorBytes)
		}
		items = append(items, model.ProviderCallSummary{Provider: execution.provider, KeyAlias: execution.keyAlias, Status: execution.status, ErrorType: execution.errorType, Error: message, LatencyMS: execution.latencyMS, ResultCount: len(execution.results), Cached: false})
	}
	return items
}

func extractUsageMeasurements(executions []extractProviderExecution) []model.UsageMeasurement {
	var items []model.UsageMeasurement
	for _, execution := range executions {
		for _, attempt := range execution.attempts {
			items = append(items, attempt.Usage...)
		}
	}
	return items
}

func extractProvidersQueried(executions []extractProviderExecution) []string {
	items := make([]string, 0, len(executions))
	for _, execution := range executions {
		items = append(items, execution.provider)
	}
	return items
}

func extractCallLogs(executions []extractProviderExecution) []model.ProviderCallLog {
	items := []model.ProviderCallLog{}
	for _, execution := range executions {
		if len(execution.attempts) == 0 {
			message := ""
			if execution.err != nil {
				message, _ = truncateExtractString(execution.err.Error(), maxExtractLogErrorBytes)
			}
			item := model.ProviderCallLog{ProviderKeyID: execution.key.ID, ProviderName: execution.provider, KeyAlias: execution.keyAlias, AttemptIndex: 1, Status: execution.status, ErrorType: execution.errorType, ErrorMessage: message, LatencyMS: execution.latencyMS, ResultCount: len(execution.results), Cached: false}
			items = append(items, boundExtractCallLog(item))
			continue
		}
		for _, attempt := range execution.attempts {
			message := ""
			if attempt.Err != nil {
				message, _ = truncateExtractString(attempt.Err.Error(), maxExtractLogErrorBytes)
			}
			attemptIndex := attempt.AttemptIndex
			if attemptIndex <= 0 {
				attemptIndex = 1
			}
			item := model.ProviderCallLog{ProviderKeyID: attempt.Key.ID, ProviderName: execution.provider, KeyAlias: attempt.KeyAlias, AttemptIndex: attemptIndex, WillRetry: attempt.WillRetry, Status: attempt.Status, ErrorType: attempt.ErrorType, ErrorMessage: message, LatencyMS: attempt.LatencyMS, ResultCount: attempt.ResultCount, Cached: false, Usage: attempt.Usage}
			items = append(items, boundExtractCallLog(item))
		}
	}
	return items
}

func boundExtractCallLog(item model.ProviderCallLog) model.ProviderCallLog {
	item.ProviderName, _ = truncateExtractString(item.ProviderName, 64)
	item.KeyAlias, _ = truncateExtractString(item.KeyAlias, 256)
	item.Status, _ = truncateExtractString(item.Status, 64)
	item.ErrorType, _ = truncateExtractString(item.ErrorType, 64)
	item.ErrorMessage, _ = truncateExtractString(item.ErrorMessage, maxExtractLogErrorBytes)
	return item
}

type extractProviderResultLog struct {
	Provider      string                 `json:"provider"`
	KeyAlias      string                 `json:"key_alias,omitempty"`
	Status        string                 `json:"status"`
	ErrorType     string                 `json:"error_type,omitempty"`
	Error         string                 `json:"error,omitempty"`
	LatencyMS     int64                  `json:"latency_ms"`
	ResultCount   int                    `json:"result_count"`
	Results       []extractResultLog     `json:"results"`
	FailedResults []model.ExtractFailure `json:"failed_results,omitempty"`
}

type extractResponseLog struct {
	Results         []extractResultLog          `json:"results"`
	FailedResults   []model.ExtractFailure      `json:"failed_results,omitempty"`
	Providers       []model.ProviderCallSummary `json:"providers"`
	Usage           []model.UsageMeasurement    `json:"usage,omitempty"`
	Meta            model.ExtractMetadata       `json:"meta"`
	ProviderResults []extractProviderResultLog  `json:"provider_results,omitempty"`
	ProviderCalls   []model.ProviderCallLog     `json:"provider_calls,omitempty"`
	Truncated       bool                        `json:"truncated,omitempty"`
}

// Extract responses can contain entire pages. Request logs retain a bounded
// preview so a parallel 20-URL request cannot silently bloat PostgreSQL by
// storing the same full body in both merged and per-provider result groups.
type extractResultLog struct {
	model.ExtractResult
	LogContentTruncated bool `json:"log_content_truncated,omitempty"`
	RawOmitted          bool `json:"raw_omitted,omitempty"`
}

func extractResponseLogPayload(response model.ExtractResponse, executions []extractProviderExecution) extractResponseLog {
	providerResults := make([]extractProviderResultLog, 0, len(executions))
	for _, execution := range executions {
		message := ""
		if execution.err != nil {
			message, _ = truncateExtractString(execution.err.Error(), maxExtractLogErrorBytes)
		}
		providerName, _ := truncateExtractString(execution.provider, 64)
		keyAlias, _ := truncateExtractString(execution.keyAlias, 256)
		status, _ := truncateExtractString(execution.status, 64)
		errorType, _ := truncateExtractString(execution.errorType, 64)
		providerResults = append(providerResults, extractProviderResultLog{Provider: providerName, KeyAlias: keyAlias, Status: status, ErrorType: errorType, Error: message, LatencyMS: execution.latencyMS, ResultCount: len(execution.results), Results: extractResultsForLog(execution.results), FailedResults: execution.failedResults})
	}
	return extractResponseLog{Results: extractResultsForLog(response.Results), FailedResults: response.FailedResults, Providers: response.Providers, Usage: response.Usage, Meta: response.Meta, ProviderResults: providerResults, ProviderCalls: extractCallLogs(executions)}
}

func marshalExtractResponseLog(response model.ExtractResponse, executions []extractProviderExecution) []byte {
	payload := extractResponseLogPayload(response, executions)
	encoded, err := json.Marshal(payload)
	if err == nil && len(encoded) <= maxExtractLogJSONBytes {
		return encoded
	}

	// Provider calls are stored in their own rows, and provider results repeat
	// the merged response. Drop those duplicates first when the JSON snapshot
	// would exceed its hard database budget.
	payload.ProviderResults = nil
	payload.ProviderCalls = nil
	payload.Truncated = true
	encoded, err = json.Marshal(payload)
	if err == nil && len(encoded) <= maxExtractLogJSONBytes {
		return encoded
	}

	for index := range payload.Results {
		payload.Results[index].Content = ""
		payload.Results[index].LogContentTruncated = true
		payload.Results[index].Images = nil
		payload.Results[index].ImagesTruncated = true
	}
	encoded, err = json.Marshal(payload)
	if err == nil && len(encoded) <= maxExtractLogJSONBytes {
		return encoded
	}
	return []byte(`{"truncated":true,"error":"extract log snapshot exceeded size budget"}`)
}

func extractResultsForLog(results []model.ExtractResult) []extractResultLog {
	items := make([]extractResultLog, 0, len(results))
	for _, result := range results {
		item := extractResultLog{ExtractResult: result}
		if content, truncated := truncateExtractString(item.Content, maxExtractLogContentBytes); truncated {
			item.Content = content
			item.LogContentTruncated = true
		}
		item.Images, item.ImagesTruncated = boundExtractImages(item.Images, 8, maxExtractLogImageBytes, item.ImagesTruncated)
		if item.Raw != nil {
			item.Raw = nil
			item.RawOmitted = true
		}
		items = append(items, item)
	}
	return items
}
