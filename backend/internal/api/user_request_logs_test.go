package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/vihor3/searchmeld/backend/internal/config"
	"github.com/vihor3/searchmeld/backend/internal/model"
	"github.com/vihor3/searchmeld/backend/internal/provider"
	"github.com/vihor3/searchmeld/backend/internal/search"
)

type userRequestTestStore struct {
	*policyTestStore
	entries       []model.UserRequestLogInput
	rejected      []model.SearchLogInput
	events        []string
	entryErr      error
	executionErr  error
	providerErr   error
	settingsErr   error
	rejectedErr   error
	cachePayload  []byte
	recordHook    func(context.Context, model.UserRequestLogInput)
	listLimit     int
	listRows      []model.UserRequestLog
	readErr       error
	detail        model.UserRequestLog
	executionID   *int64
	detailReads   []int64
	providerRetry int
}

// RecordUserRequestLog captures metadata after handling and optionally inspects
// its detached context; injected failures never simulate accounting writes.
func (s *userRequestTestStore) RecordUserRequestLog(ctx context.Context, input model.UserRequestLogInput) error {
	s.entries = append(s.entries, input)
	s.events = append(s.events, "entry")
	if s.recordHook != nil {
		s.recordHook(ctx, input)
	}
	return s.entryErr
}

// RecordRejectedRequestLog retains the existing denial write separately so tests
// can assert its correlation and admission exemption even when either log fails.
func (s *userRequestTestStore) RecordRejectedRequestLog(_ context.Context, input model.SearchLogInput) error {
	s.rejected = append(s.rejected, input)
	s.events = append(s.events, "rejected")
	return s.rejectedErr
}

// RecordSearchLog counts only selected execution writes, retaining attempts and
// allowing accounting failure to keep its original stronger response semantics.
func (s *userRequestTestStore) RecordSearchLog(_ context.Context, input model.SearchLogInput) error {
	s.logs = append(s.logs, input)
	s.events = append(s.events, "execution")
	return s.executionErr
}

// MarkAPITokenUsed records ordering as well as the original admission count.
func (s *userRequestTestStore) MarkAPITokenUsed(ctx context.Context, id int64) error {
	s.events = append(s.events, "mark")
	return s.scopeAuthStore.MarkAPITokenUsed(ctx, id)
}

// RuntimeSettings permits focused pre-auth failure cases without provider work.
func (s *userRequestTestStore) RuntimeSettings(context.Context) (model.RuntimeSettings, error) {
	return s.settings, s.settingsErr
}

// ListProviders enables synthetic Search/Extract adapters, with optional lookup
// failure and retry configuration to exercise selected-but-unwritten executions.
func (s *userRequestTestStore) ListProviders(context.Context) ([]model.ProviderConfig, error) {
	s.providerReads++
	return []model.ProviderConfig{
		{Name: model.ProviderBrave, Enabled: true, AvailableKeys: 1, Settings: map[string]interface{}{"key_retry_count": s.providerRetry}},
		{Name: model.ProviderTavily, Enabled: true, AvailableKeys: 1, Settings: map[string]interface{}{"key_retry_count": 0}},
	}, s.providerErr
}

// GetCache injects a cache hit without bypassing the real orchestrator's per-call
// execution write; the metadata observer must not create a provider attempt.
func (s *userRequestTestStore) GetCache(context.Context, string) ([]byte, bool, error) {
	s.cacheReads++
	return s.cachePayload, s.cachePayload != nil, nil
}

// ListUserRequestLogs exposes exactly the requested window for admin limit tests.
func (s *userRequestTestStore) ListUserRequestLogs(_ context.Context, limit int) ([]model.UserRequestLog, error) {
	s.listLimit = limit
	return s.listRows, s.readErr
}

// GetUserRequestLog supplies a scalar link without any execution payload lookup.
func (s *userRequestTestStore) GetUserRequestLog(_ context.Context, id int64) (model.UserRequestLog, *int64, error) {
	s.detailReads = append(s.detailReads, id)
	return s.detail, s.executionID, s.readErr
}

type userRequestTestFixture struct {
	store  *userRequestTestStore
	auth   *AuthService
	h      *Handler
	server *Server
	pool   *policyTestKeyPool
	brave  *policyTestProvider
	tavily *mcpLimitExtractProvider
	log    *rejectedLogTestLogger
}

// newUserRequestTestFixture mounts production middleware and all covered routes
// over synthetic providers, credential-aware lookup and independently captured logs.
func newUserRequestTestFixture(t *testing.T, bodyLimit int64) *userRequestTestFixture {
	t.Helper()
	store := &userRequestTestStore{policyTestStore: &policyTestStore{scopeAuthStore: &scopeAuthStore{
		settings: model.RuntimeSettings{
			APIAuthRequired: true, CompatTavilyEnabled: true, CompatSerperEnabled: true, CompatOpenAIEnabled: true,
			AllowPrivateExtractTargets: true, DefaultProviders: []string{model.ProviderBrave}, SearchLogsLimit: 37,
		},
		token:         model.APIToken{ID: 42, Name: "request-time name", Scopes: []string{"search", "extract"}},
		expectedToken: adminTestToken,
	}}}
	brave := &policyTestProvider{name: model.ProviderBrave}
	tavily := &mcpLimitExtractProvider{policyTestProvider: policyTestProvider{name: model.ProviderTavily}, content: "synthetic page content"}
	pool := &policyTestKeyPool{}
	orchestrator := search.NewOrchestrator(provider.NewRegistry(brave, tavily), pool, store)
	auth := NewAuthService(store, 0, 0, 0, 0)
	h := NewHandler(store, auth, orchestrator)
	h.EnableMCP("/custom/mcp")
	log := &rejectedLogTestLogger{}
	h.SetLogger(log)
	server := NewServer(config.Config{RequestBodyLimitBytes: bodyLimit}, log)
	server.Mount(h.Mount)
	return &userRequestTestFixture{store: store, auth: auth, h: h, server: server, pool: pool, brave: brave, tavily: tavily, log: log}
}

// onlyUserRequestEntry requires one finalized row with the actual status and
// server ID, and valid elapsed/start timestamps without depending on clock speed.
func onlyUserRequestEntry(t *testing.T, f *userRequestTestFixture, response *httptest.ResponseRecorder) model.UserRequestLogInput {
	t.Helper()
	if len(f.store.entries) != 1 {
		t.Fatalf("entry writes = %d, want 1", len(f.store.entries))
	}
	entry := f.store.entries[0]
	if entry.RequestID == "" || entry.RequestID != response.Header().Get("X-Request-ID") {
		t.Fatal("entry lost the server response identity")
	}
	if entry.HTTPStatus == nil || *entry.HTTPStatus != response.Code || entry.LatencyMS < 0 || entry.CreatedAt.IsZero() || entry.CreatedAt.After(time.Now()) {
		t.Fatal("entry has incorrect status or timing")
	}
	return entry
}

// TestMountedUserRequestRouteMatrix requires exactly one entry for every native
// and compatibility POST, including rejected bodies and credentials. Lookup and
// admission counts prove that observing identity does not authenticate twice.
func TestMountedUserRequestRouteMatrix(t *testing.T) {
	for _, route := range []struct {
		path      string
		operation string
		format    string
		body      string
	}{
		{"/v1/search", "search", "native", `{"query":"fixture query","providers":["brave"]}`},
		{"/v1/extract", "extract", "native", `{"urls":["https://example.com"],"providers":["tavily"]}`},
		{"/v1/compat/tavily/search", "search", "tavily", `{"query":"fixture query","providers":["brave"]}`},
		{"/v1/compat/tavily/extract", "extract", "tavily", `{"urls":["https://example.com"],"providers":["tavily"]}`},
		{"/v1/compat/serper/search", "search", "serper", `{"q":"fixture query","providers":["brave"]}`},
		{"/v1/compat/openai/responses-search", "search", "openai", `{"input":"fixture query","providers":["brave"]}`},
	} {
		for _, variant := range []string{"success", "missing", "invalid", "malformed", "empty", "oversized", "admin", "auth off", "rate", "disabled", "provider lookup failure", "entry failure"} {
			if variant == "disabled" && route.format == "native" {
				continue
			}
			t.Run(route.path+"/"+variant, func(t *testing.T) {
				f := newUserRequestTestFixture(t, 1024)
				body, credential := route.body, adminTestToken
				status, authType, marks, lookups, keys := 200, "api_token", 1, 1, 1
				selected := true
				switch variant {
				case "missing":
					credential, status, authType, marks, lookups, keys, selected = "", 401, "unknown", 0, 0, 0, false
				case "invalid":
					credential, status, authType, marks, selected = "invalid-fixture-credential", 401, "unknown", 0, false
				case "malformed":
					body, status, selected = "{", 400, false
				case "empty":
					body, status, selected = "", 400, false
				case "oversized":
					body, status, selected = strings.Repeat("x", 2048), 400, false
				case "admin":
					credential, authType, marks, lookups = adminTestKey, "admin_key", 0, 0
				case "auth off":
					f.store.settings.APIAuthRequired = false
					credential, authType, marks, lookups, keys = "unvalidated-fixture", "anonymous", 0, 0, 0
				case "rate":
					f.store.token.RateLimitPerMin = 1
					f.auth.rateWindows[42] = rateWindow{StartedAt: time.Now(), Count: 1}
					status, selected = 429, false
				case "disabled":
					f.store.settings.CompatTavilyEnabled = false
					f.store.settings.CompatSerperEnabled = false
					f.store.settings.CompatOpenAIEnabled = false
					status, selected = 404, false
				case "provider lookup failure":
					f.store.providerErr = errors.New("synthetic provider store unavailable")
					status = 500
				case "entry failure":
					f.store.entryErr = errors.New("private-storage-detail-sentinel")
				}
				r := adminTestRequest(http.MethodPost, route.path+"?private-query-sentinel=1", body, nil)
				if credential != "" {
					r.Header.Set("Authorization", "Bearer "+credential)
				}
				response := adminTestResponse(t, f.server.Router(), r, status)
				entry := onlyUserRequestEntry(t, f, response)
				if entry.Path != route.path || entry.Method != http.MethodPost || entry.Operation != route.operation || entry.CompatFormat != route.format || entry.AuthType != authType || entry.Completion != "completed" {
					t.Fatal("route classification or completion changed")
				}
				if authType == "api_token" {
					if entry.APITokenID == nil || *entry.APITokenID != 42 || entry.TokenName != "request-time name" {
						t.Fatal("verified Token snapshot missing, including on rate rejection")
					}
				} else if entry.APITokenID != nil || entry.TokenName != "" {
					t.Fatal("unverified caller was attributed to a Token")
				}
				if (entry.ExecutionRequestID != nil) != selected {
					t.Fatal("intended execution was inferred at the wrong boundary")
				}
				if selected && *entry.ExecutionRequestID != entry.RequestID {
					t.Fatal("native/compat execution correlation is not exact")
				}
				if f.store.usageMarks != marks || f.store.tokenLookups != lookups || f.store.adminKeyLookups != keys {
					t.Fatalf("admission/lookups changed: marks=%d Token=%d Key=%d", f.store.usageMarks, f.store.tokenLookups, f.store.adminKeyLookups)
				}
				wantExecutions := 0
				if selected && variant != "provider lookup failure" {
					wantExecutions = 1
				}
				if len(f.store.logs) != wantExecutions || len(f.store.rejected) != 0 || entry.MCPErrorCount != 0 || entry.MCPToolErrorCount != 0 {
					t.Fatal("entry logging changed execution writes or invented MCP outcomes")
				}
				if !selected && f.pool.acquired.Load() != 0 {
					t.Fatal("pre-execution rejection acquired an upstream key")
				}
				if variant == "entry failure" {
					if len(f.log.errors) != 1 || f.log.errors[0].message != "user_request_log_failed" || f.log.errors[0].fields["error"] != "storage_error" {
						t.Fatal("entry failure was not reported safely")
					}
				}
			})
		}
	}
}

// TestUserRequestExtractDenialsPreserveAccounting checks native and body-key
// authentication, fixed safe denial records, and independent failure of each log.
func TestUserRequestExtractDenialsPreserveAccounting(t *testing.T) {
	for _, path := range []string{"/v1/extract", "/v1/compat/tavily/extract"} {
		for _, denial := range []string{"scope", "provider"} {
			for _, failure := range []string{"none", "entry", "rejected"} {
				t.Run(path+"/"+denial+"/"+failure, func(t *testing.T) {
					f := newUserRequestTestFixture(t, 1024)
					f.store.token.AllowedProviders = []string{model.ProviderBrave}
					if denial == "scope" {
						f.store.token.Scopes = []string{"search"}
					}
					if failure == "entry" {
						f.store.entryErr = errors.New("entry unavailable")
					} else if failure == "rejected" {
						f.store.rejectedErr = errors.New("rejection unavailable")
					}
					body := `{"urls":["https://example.com/private-body-sentinel"],"providers":["tavily"],"api_key":"` + adminTestToken + `"}`
					r := adminTestRequest(http.MethodPost, path, body, nil)
					if path == "/v1/extract" {
						r.Header.Set("Authorization", "Bearer "+adminTestToken)
					}
					response := adminTestResponse(t, f.server.Router(), r, http.StatusForbidden)
					entry := onlyUserRequestEntry(t, f, response)
					if entry.AuthType != "api_token" || entry.APITokenID == nil || *entry.APITokenID != 42 || entry.ExecutionRequestID == nil || *entry.ExecutionRequestID != entry.RequestID {
						t.Fatal("Extract denial lost verified identity or exact rejected-execution link")
					}
					if !reflect.DeepEqual(f.store.events, []string{"rejected", "entry"}) || f.store.usageMarks != 0 || len(f.store.logs) != 0 || f.pool.acquired.Load() != 0 {
						t.Fatal("Extract denial changed accounting or persistence order")
					}
					if len(f.store.rejected) != 1 || f.store.rejected[0].RequestID != entry.RequestID || string(f.store.rejected[0].RequestJSON) != "{}" || string(f.store.rejected[0].ResponseJSON) != "{}" {
						t.Fatal("existing metadata-only rejection record changed")
					}
					if denial == "scope" && response.Body.String() != "{\"error\":{\"message\":\"api token does not include extract scope\",\"status\":403}}\n" {
						t.Fatal("scope rejection wire envelope changed")
					}
					if denial == "provider" && path != "/v1/extract" && !strings.HasPrefix(response.Body.String(), `{"detail":{"error":`) {
						t.Fatal("Tavily provider denial lost its wire envelope")
					}
					assertUserRequestPrivacy(t, entry, []string{adminTestToken, "private-body-sentinel"})
				})
			}
		}
	}
}

// assertUserRequestPrivacy checks the new metadata only; existing execution
// payloads deliberately retain their separate request/result logging contract.
func assertUserRequestPrivacy(t *testing.T, entry model.UserRequestLogInput, sentinels []string) {
	t.Helper()
	payload, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("encode entry: %v", err)
	}
	for _, sentinel := range sentinels {
		if bytes.Contains(payload, []byte(sentinel)) {
			t.Fatal("entry copied an excluded credential or payload field")
		}
	}
}

// TestMountedUserRequestMCPMatrix keeps transport status separate from typed RPC
// and tool failures on every configured alias. Notifications and rejected batches
// never acquire execution identity, while a dispatched tool uses its exact ID.
func TestMountedUserRequestMCPMatrix(t *testing.T) {
	tool := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"fixture","providers":["brave"]}}}`
	for _, path := range []string{"/custom/mcp", "/custom/mcp/", "/v1/mcp", "/v1/mcp/"} {
		for _, test := range []struct {
			name       string
			method     string
			body       string
			credential string
			status     int
			authType   string
			rpcErrors  int
			toolErrors int
			selected   bool
			marks      int
		}{
			{"info", "GET", "", adminTestToken, 200, "anonymous", 0, 0, false, 0},
			{"sse", "GET", "", adminTestToken, 405, "anonymous", 0, 0, false, 0},
			{"delete", "DELETE", "", adminTestToken, 405, "anonymous", 0, 0, false, 0},
			{"discovery", "POST", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, adminTestToken, 200, "anonymous", 0, 0, false, 0},
			{"empty", "POST", "", "", 400, "unknown", 1, 0, false, 0},
			{"malformed", "POST", "{", "", 400, "unknown", 1, 0, false, 0},
			{"invalid version", "POST", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, "", 200, "anonymous", 1, 0, false, 0},
			{"unknown method", "POST", `{"jsonrpc":"2.0","id":1,"method":"private-method-sentinel"}`, adminTestToken, 200, "api_token", 1, 0, false, 1},
			{"unknown tool", "POST", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"private-tool-sentinel"}}`, adminTestToken, 200, "api_token", 1, 0, false, 1},
			{"invalid arguments", "POST", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{}}}`, adminTestToken, 200, "api_token", 1, 0, false, 1},
			{"missing credential", "POST", tool, "", 401, "unknown", 1, 0, false, 0},
			{"invalid credential", "POST", tool, "invalid-fixture", 401, "unknown", 1, 0, false, 0},
			{"scope", "POST", tool, adminTestToken, 200, "api_token", 1, 0, false, 1},
			{"provider policy", "POST", tool, adminTestToken, 200, "api_token", 1, 0, false, 1},
			{"rate", "POST", tool, adminTestToken, 429, "api_token", 1, 0, false, 1},
			{"success", "POST", tool, adminTestToken, 200, "api_token", 0, 0, true, 1},
			{"admin", "POST", tool, adminTestKey, 200, "admin_key", 0, 0, true, 0},
			{"auth off", "POST", tool, "unvalidated-fixture", 200, "anonymous", 0, 0, true, 0},
			{"tool failure", "POST", tool, adminTestToken, 200, "api_token", 0, 1, true, 1},
			{"notification", "POST", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, adminTestToken, 202, "anonymous", 0, 0, false, 0},
			{"tool notification", "POST", `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"search","arguments":{"query":"ignored"}}}`, adminTestToken, 202, "api_token", 0, 0, false, 1},
			{"multi tool", "POST", "[" + tool + "," + tool + "]", adminTestToken, 400, "unknown", 1, 0, false, 0},
			{"mixed success", "POST", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"},` + tool + `,{"jsonrpc":"2.0","method":"notifications/initialized"}]`, adminTestToken, 200, "api_token", 0, 0, true, 1},
			{"mixed failure", "POST", `[{"jsonrpc":"1.0","id":1,"method":"ping"},` + tool + `,{"jsonrpc":"2.0","id":3,"method":"private-method-sentinel"}]`, adminTestToken, 200, "api_token", 2, 1, true, 1},
		} {
			t.Run(path+"/"+test.name, func(t *testing.T) {
				f := newUserRequestTestFixture(t, 1024*1024)
				switch test.name {
				case "scope":
					f.store.token.Scopes = []string{"extract"}
				case "provider policy":
					f.store.token.AllowedProviders = []string{model.ProviderTavily}
				case "rate":
					f.store.token.RateLimitPerMin = 1
					f.auth.rateWindows[42] = rateWindow{StartedAt: time.Now(), Count: 1}
				case "auth off":
					f.store.settings.APIAuthRequired = false
				case "tool failure", "mixed failure":
					f.store.providerErr = errors.New("private-provider-error-sentinel")
				}
				r := adminTestRequest(test.method, path+"?private-query-sentinel=1", test.body, nil)
				if test.credential != "" {
					r.Header.Set("Authorization", "Bearer "+test.credential)
				}
				if test.name == "sse" {
					r.Header.Set("Accept", "text/event-stream")
				}
				response := adminTestResponse(t, f.server.Router(), r, test.status)
				entry := onlyUserRequestEntry(t, f, response)
				if entry.Operation != "mcp" || entry.CompatFormat != "native" || entry.Method != test.method || entry.Path != path || entry.AuthType != test.authType || entry.Completion != "completed" {
					t.Fatal("MCP route or identity classification changed")
				}
				if entry.MCPErrorCount != test.rpcErrors || entry.MCPToolErrorCount != test.toolErrors || (entry.ExecutionRequestID != nil) != test.selected || f.store.usageMarks != test.marks {
					t.Fatalf("MCP observations: rpc=%d tool=%d selected=%t marks=%d", entry.MCPErrorCount, entry.MCPToolErrorCount, entry.ExecutionRequestID != nil, f.store.usageMarks)
				}
				if test.authType == "api_token" && (entry.APITokenID == nil || *entry.APITokenID != 42 || entry.TokenName != "request-time name") {
					t.Fatal("MCP lost verified Token attribution before rate/scope denial")
				}
				if test.authType != "api_token" && (entry.APITokenID != nil || entry.TokenName != "") {
					t.Fatal("public or rejected MCP request borrowed Token identity")
				}
				if test.authType == "anonymous" && (f.store.tokenLookups != 0 || f.store.adminKeyLookups != 0) {
					t.Fatal("public MCP discovery or auth bypass performed a credential lookup")
				}
				if f.store.tokenLookups > 1 || f.store.adminKeyLookups > 1 || len(f.store.rejected) != 0 {
					t.Fatal("MCP introduced an extra lookup or Extract rejection write")
				}
				if !test.selected {
					if len(f.store.logs) != 0 || f.pool.acquired.Load() != 0 {
						t.Fatal("MCP notification/rejection executed a tool")
					}
				} else if test.toolErrors == 0 {
					if len(f.store.logs) != 1 || f.store.logs[0].RequestID != *entry.ExecutionRequestID || *entry.ExecutionRequestID == entry.RequestID {
						t.Fatal("MCP invocation is not correlated to its exact selected execution")
					}
					wantEvents := []string{"execution", "entry"}
					if test.marks != 0 {
						wantEvents = []string{"mark", "execution", "entry"}
					}
					if !reflect.DeepEqual(f.store.events, wantEvents) {
						t.Fatal("MCP immediate usage mark moved after execution or entry recording")
					}
				} else if len(f.store.logs) != 0 {
					t.Fatal("fixture should select an execution whose initial store read fails")
				}
				if test.status == 202 && (response.Body.Len() != 0 || response.Header().Get("Mcp-Protocol-Version") != mcpLatestProtocolVersion) {
					t.Fatal("MCP notification wire acceptance changed")
				}
				assertUserRequestPrivacy(t, entry, []string{adminTestToken, adminTestKey, "private-query-sentinel", "private-method-sentinel", "private-tool-sentinel", "private-provider-error-sentinel"})
			})
		}
	}
}

// TestUserRequestMCPBoundsAndCancellation exercises finite error aggregation and
// encoding/input rejections without retaining large client IDs or response content.
func TestUserRequestMCPBoundsAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name       string
		count      int
		status     int
		errorCount int
	}{
		{"empty batch", 0, 400, 1},
		{"maximum errors", maxMCPBatchRequests, 200, maxMCPBatchRequests},
		{"oversized batch", maxMCPBatchRequests + 1, 400, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newUserRequestTestFixture(t, 1024*1024)
			requests := make([]mcpRequest, test.count)
			for index := range requests {
				requests[index] = mcpRequest{JSONRPC: "invalid", ID: json.RawMessage(fmt.Sprint(index)), Method: "ping"}
			}
			body, err := json.Marshal(requests)
			if err != nil {
				t.Fatal(err)
			}
			response := adminTestResponse(t, f.server.Router(), adminTestRequest("POST", "/custom/mcp", string(body), nil), test.status)
			entry := onlyUserRequestEntry(t, f, response)
			if entry.MCPErrorCount != test.errorCount || entry.MCPToolErrorCount != 0 || entry.ExecutionRequestID != nil {
				t.Fatal("MCP error aggregation exceeded or lost the bounded dispatch outcomes")
			}
		})
	}
	for _, name := range []string{"oversized input", "oversized output", "oversized auth error", "canceled before read", "canceled during read"} {
		t.Run(name, func(t *testing.T) {
			f := newUserRequestTestFixture(t, 1024*1024)
			body := `{"jsonrpc":"2.0","id":"` + strings.Repeat("<", 50000) + `","method":"tools/list"}`
			status, completion := http.StatusRequestEntityTooLarge, "completed"
			if name == "oversized input" {
				body, status = strings.Repeat("x", 1024*1024+1), http.StatusBadRequest
			} else if name == "oversized auth error" {
				body = `{"jsonrpc":"2.0","id":"` + strings.Repeat("<", 50000) + `","method":"tools/call"}`
			}
			r := adminTestRequest("POST", "/custom/mcp", body, nil)
			if strings.HasPrefix(name, "canceled") {
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()
				r = r.WithContext(ctx)
				status, completion = http.StatusRequestTimeout, "canceled"
				if name == "canceled before read" {
					cancel()
					r.Body = &userRequestUnreadBody{t: t}
				} else {
					r.Body = &cancelingMCPBody{cancel: cancel}
				}
			}
			response := adminTestResponse(t, f.server.Router(), r, status)
			entry := onlyUserRequestEntry(t, f, response)
			if entry.MCPErrorCount != 1 || entry.MCPToolErrorCount != 0 || entry.Completion != completion || entry.ExecutionRequestID != nil || response.Body.Len() > 256 {
				t.Fatal("MCP transport failure was misclassified or reflected oversized content")
			}
		})
	}
}

type userRequestUnreadBody struct{ t *testing.T }

// Read fails if metadata capture consumes a body before the existing handler does.
func (b *userRequestUnreadBody) Read([]byte) (int, error) {
	b.t.Fatal("entry logger or rejected handler unexpectedly read the body")
	return 0, errors.New("unexpected read")
}

// Close owns no external resource and does not consume the synthetic body.
func (*userRequestUnreadBody) Close() error { return nil }

// TestUserRequestMetadataPrivacyAndPeerTrust excludes credential-bearing fields,
// preserves the permitted caller prefix and verified Unicode name, and applies
// only the established loopback peer rule to source IP attribution.
func TestUserRequestMetadataPrivacyAndPeerTrust(t *testing.T) {
	for _, test := range []struct {
		name   string
		peer   string
		realIP []string
		wantIP string
	}{
		{"untrusted forwarding", "192.0.2.10:54321", []string{"198.51.100.5"}, "192.0.2.10"},
		{"trusted loopback", "127.0.0.1:54321", []string{"198.51.100.5"}, "198.51.100.5"},
		{"repeated header", "127.0.0.1:54321", []string{"198.51.100.5", "203.0.113.7"}, "127.0.0.1"},
		{"invalid forwarding", "127.0.0.1:54321", []string{"private-ip-sentinel"}, "127.0.0.1"},
		{"ipv6 peer", "[2001:0db8::1]:54321", []string{"198.51.100.5"}, "2001:db8::1"},
		{"invalid peer", "private-peer-sentinel", nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newUserRequestTestFixture(t, 1024)
			f.store.token.Name = "display\x00\n\xff\ufffd" + strings.Repeat("\u754c", 100)
			f.store.token.Token = "private-plaintext-sentinel"
			f.store.token.TokenHash = "private-hash-sentinel"
			r := adminTestRequest("POST", "/v1/search?private-query-sentinel=1", `{"raw":"private-body-sentinel"}`, &http.Cookie{Name: adminSessionCookieName, Value: "private-cookie-sentinel"})
			r.RemoteAddr = test.peer
			r.Header["X-Real-Ip"] = test.realIP
			r.Header.Set("X-Forwarded-For", "203.0.113.7")
			r.Header.Set("Forwarded", "for=203.0.113.7")
			r.Header.Set("Authorization", "Bearer "+adminTestToken)
			r.Header.Set("X-API-Key", "private-header-sentinel")
			r.Header.Set("X-Request-ID", "safe/correlation?!")
			response := adminTestResponse(t, f.server.Router(), r, http.StatusBadRequest)
			entry := onlyUserRequestEntry(t, f, response)
			if entry.ClientIP != test.wantIP || entry.Path != "/v1/search" || !strings.HasPrefix(entry.RequestID, "safecorrelation-") {
				t.Fatal("trusted peer, safe pathname or permitted correlation prefix changed")
			}
			if !utf8.ValidString(entry.TokenName) || !strings.HasPrefix(entry.TokenName, "display\ufffd") || len(entry.TokenName) > model.UserRequestLogTokenNameMaxBytes || strings.ContainsAny(entry.TokenName, "\x00\n") {
				t.Fatal("Token name was not bounded at a valid Unicode boundary")
			}
			assertUserRequestPrivacy(t, entry, []string{adminTestToken, "private-cookie-sentinel", "private-query-sentinel", "private-body-sentinel", "private-header-sentinel", "private-plaintext-sentinel", "private-hash-sentinel", "private-peer-sentinel", "private-ip-sentinel"})
			f.store.token.Name = "renamed later"
			if f.store.entries[0].TokenName != entry.TokenName {
				t.Fatal("request-time name snapshot followed a later Token mutation")
			}
		})
	}
	for _, path := range []string{"/v1/compat/tavily/search", "/v1/compat/tavily/extract"} {
		f := newUserRequestTestFixture(t, 1024)
		body := `{"query":"fixture","urls":["https://example.com"],"providers":["tavily"],"api_key":"` + adminTestToken + `"}`
		response := adminTestResponse(t, f.server.Router(), adminTestRequest("POST", path, body, nil), 200)
		entry := onlyUserRequestEntry(t, f, response)
		if entry.AuthType != "api_token" || f.store.tokenLookups != 1 || f.store.usageMarks != 1 {
			t.Fatal("Tavily body-key attribution or admission changed")
		}
		assertUserRequestPrivacy(t, entry, []string{adminTestToken, "synthetic page content"})
	}
}

// TestUserRequestNoBodyAndRouteExclusions covers the pre-auth boundary, Tavily's
// existing bounded body adapter, preflight and unmatched/admin traffic exclusions.
func TestUserRequestNoBodyAndRouteExclusions(t *testing.T) {
	f := newUserRequestTestFixture(t, 64)
	r := adminTestRequest("POST", "/v1/search", "", nil)
	r.Body = &userRequestUnreadBody{t: t}
	response := adminTestResponse(t, f.server.Router(), r, 401)
	entry := onlyUserRequestEntry(t, f, response)
	if entry.AuthType != "unknown" || entry.ExecutionRequestID != nil || f.store.tokenLookups != 0 {
		t.Fatal("pre-auth rejection invented identity or execution")
	}
	for _, path := range []string{"/v1/compat/tavily/search", "/v1/compat/tavily/extract"} {
		f := newUserRequestTestFixture(t, 64)
		response := adminTestResponse(t, f.server.Router(), adminTestRequest("POST", path, strings.Repeat("x", 128), nil), 400)
		entry := onlyUserRequestEntry(t, f, response)
		if entry.AuthType != "unknown" || f.store.adminKeyLookups != 0 || f.store.tokenLookups != 0 || response.Body.String() != "{\"error\":{\"message\":\"invalid body\",\"status\":400}}\n" {
			t.Fatal("logging did not wrap the Tavily adapter's original early rejection")
		}
	}
	for _, test := range []struct {
		method string
		path   string
		status int
	}{
		{"GET", "/healthz", 200},
		{"GET", "/v1/search", 405},
		{"OPTIONS", "/v1/search", 204},
		{"PATCH", "/custom/mcp", 405},
		{"POST", "/unmatched", 404},
		{"GET", "/v1/providers", 404},
		{"GET", "/api/admin/request-logs", 200},
		{"GET", "/api/admin/me", 200},
	} {
		f := newUserRequestTestFixture(t, 64)
		r := adminTestRequest(test.method, test.path, "", nil)
		r.Header.Set("Authorization", "Bearer "+adminTestKey)
		adminTestResponse(t, f.server.Router(), r, test.status)
		if len(f.store.entries) != 0 {
			t.Fatalf("entry logging escaped its mounted route/method boundary: %s %s", test.method, test.path)
		}
	}
}

// TestUserRequestSearchPolicyDenials keeps scope/provider rejection outside the
// execution path on each search transport, while preserving one admission mark.
func TestUserRequestSearchPolicyDenials(t *testing.T) {
	for _, route := range []struct{ path, field string }{
		{"/v1/search", "query"}, {"/v1/compat/tavily/search", "query"},
		{"/v1/compat/serper/search", "q"}, {"/v1/compat/openai/responses-search", "input"},
	} {
		for _, scopeDenied := range []bool{false, true} {
			f := newUserRequestTestFixture(t, 1024)
			f.store.token.AllowedProviders = []string{model.ProviderTavily}
			if scopeDenied {
				f.store.token.Scopes = []string{"extract"}
			}
			body := fmt.Sprintf(`{"%s":"fixture","providers":["brave"]}`, route.field)
			r := adminTestRequest("POST", route.path, body, nil)
			r.Header.Set("Authorization", "Bearer "+adminTestToken)
			response := adminTestResponse(t, f.server.Router(), r, 403)
			entry := onlyUserRequestEntry(t, f, response)
			if entry.AuthType != "api_token" || entry.ExecutionRequestID != nil || f.store.usageMarks != 1 || f.pool.acquired.Load() != 0 || !reflect.DeepEqual(f.store.events, []string{"mark", "entry"}) {
				t.Fatal("search denial changed execution or admission behavior")
			}
		}
	}
}

type userRequestRetryProvider struct {
	policyTestProvider
	attempts  int
	failUntil int
}

// Search fails the configured initial attempts with a retryable provider error,
// then delegates successful result construction to the existing synthetic adapter.
func (p *userRequestRetryProvider) Search(ctx context.Context, req model.SearchRequest, key model.APIKey) (model.ProviderResponse, error) {
	p.attempts++
	if p.attempts <= p.failUntil {
		return model.ProviderResponse{}, &provider.Error{Type: provider.ErrorTypeAuth, Message: "private-upstream-error-sentinel"}
	}
	return p.policyTestProvider.Search(ctx, req, key)
}

// TestUserRequestExecutionCorrelation preserves cache, retry and accounting-error
// semantics, using equality with captured execution IDs rather than parent prefixes.
func TestUserRequestExecutionCorrelation(t *testing.T) {
	for _, mode := range []string{"cache", "retry", "provider failure", "accounting failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newUserRequestTestFixture(t, 1024)
			wantCalls, wantStatus := 1, 200
			if mode == "cache" {
				f.store.settings.CacheEnabled = true
				f.store.cachePayload = []byte(`{"results":[{"url":"https://example.com/cached"}],"meta":{"request_id":"old-cached-id"}}`)
				wantCalls = 0
			} else if mode == "retry" || mode == "provider failure" {
				f.store.providerRetry = 1
				adapter := &userRequestRetryProvider{policyTestProvider: policyTestProvider{name: model.ProviderBrave}, failUntil: 1}
				if mode == "provider failure" {
					adapter.failUntil = 2
				}
				f.h.orchestrator = search.NewOrchestrator(provider.NewRegistry(adapter), f.pool, f.store)
				wantCalls = 2
			} else {
				f.store.executionErr = errors.New("synthetic accounting write failed")
				wantStatus = 500
			}
			for attempt := 0; attempt < 2; attempt++ {
				r := adminTestRequest("POST", "/v1/search", `{"query":"fixture","providers":["brave"]}`, nil)
				r.Header.Set("Authorization", "Bearer "+adminTestToken)
				r.Header.Set("X-Request-ID", "shared-prefix")
				adminTestResponse(t, f.server.Router(), r, wantStatus)
				entry := f.store.entries[attempt]
				log := f.store.logs[attempt]
				if entry.ExecutionRequestID == nil || *entry.ExecutionRequestID != log.RequestID || entry.RequestID != log.RequestID {
					t.Fatal("native entry and execution identity were not preserved exactly")
				}
				if attempt == 0 && len(log.Calls) != wantCalls {
					t.Fatalf("provider attempts = %d, want %d", len(log.Calls), wantCalls)
				}
				if mode == "cache" && !log.CacheHit {
					t.Fatal("cached execution metadata changed")
				}
				if mode == "retry" && attempt == 0 && (!log.Calls[0].WillRetry || log.Calls[1].Status != "success") {
					t.Fatal("retry attempts were not preserved under a single execution")
				}
				if mode == "provider failure" && attempt == 0 && (log.Status != "error" || *entry.HTTPStatus != 200) {
					t.Fatal("HTTP 200 search transport was confused with provider failure")
				}
				assertUserRequestPrivacy(t, entry, []string{"old-cached-id", "private-upstream-error-sentinel"})
			}
			if f.store.entries[0].RequestID == f.store.entries[1].RequestID || len(f.store.logs) != 2 || f.store.usageMarks != 2 || !reflect.DeepEqual(f.store.events, []string{"execution", "mark", "entry", "execution", "mark", "entry"}) {
				t.Fatal("shared client prefix caused duplicate identity or changed accounting order")
			}
		})
	}
}

// TestUserRequestAdminReads exercises the mounted Cookie/Key boundary, limit and
// null envelopes, exact numeric detail selection, and secret-safe error mapping.
func TestUserRequestAdminReads(t *testing.T) {
	for _, path := range []string{"/api/admin/request-logs", "/api/admin/request-logs/7"} {
		for _, identity := range []string{"anonymous", "business Token", "Key", "Cookie", "Cookie without proof", "invalid header with Cookie", "hostile Origin"} {
			t.Run(path+"/"+identity, func(t *testing.T) {
				f := newUserRequestTestFixture(t, 1024)
				f.auth.sessions["fixture-session"] = session{Username: "operator", ExpiresAt: time.Now().Add(time.Hour)}
				r := adminTestRequest("GET", path, "", nil)
				status := 200
				switch identity {
				case "anonymous":
					status = 401
				case "business Token":
					r.Header.Set("Authorization", "Bearer "+adminTestToken)
					status = 401
				case "Key":
					r.Header.Set("X-API-Key", adminTestKey)
				default:
					r.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: "fixture-session"})
					if identity == "Cookie without proof" {
						r.Header.Del(adminBrowserHeader)
						status = 403
					} else if identity == "invalid header with Cookie" {
						r.Header.Set("Authorization", "Bearer invalid-fixture")
						status = 401
					} else if identity == "hostile Origin" {
						r.Header.Set("Origin", "https://hostile.example")
						status = 403
					}
				}
				response := adminTestResponse(t, f.server.Router(), r, status)
				if len(f.store.entries) != 0 || f.store.usageMarks != 0 || f.store.tokenLookups != 0 {
					t.Fatal("admin metadata reads leaked into business logging/authentication")
				}
				if status != 200 && (f.store.listLimit != 0 || len(f.store.detailReads) != 0) {
					t.Fatal("unauthorized read reached metadata storage")
				}
				if status == 200 && path == "/api/admin/request-logs" && response.Body.String() != "{\"logs\":[]}\n" {
					t.Fatal("empty list must be a non-null array")
				}
			})
		}
	}
	for _, test := range []struct {
		query      string
		configured int
		want       int
	}{
		{"", 37, 37}, {"?limit=invalid", 37, 37}, {"?limit=-1", 37, 37},
		{"?limit=0", 37, 37}, {"?limit=999999999999999999999", 37, 37},
		{"?limit=1", 37, 1}, {"?limit=1001", 37, 1000}, {"", 1001, 1000}, {"", 0, 100},
	} {
		f := newUserRequestTestFixture(t, 1024)
		f.store.settings.SearchLogsLimit = test.configured
		r := adminTestRequest("GET", "/api/admin/request-logs"+test.query, "", nil)
		r.Header.Set("Authorization", "Bearer "+adminTestKey)
		adminTestResponse(t, f.server.Router(), r, 200)
		if f.store.listLimit != test.want {
			t.Fatalf("list limit = %d, want %d", f.store.listLimit, test.want)
		}
	}
	for _, id := range []string{"no-id", "0", "-1", "9223372036854775808"} {
		f := newUserRequestTestFixture(t, 1024)
		r := adminTestRequest("GET", "/api/admin/request-logs/"+id, "", nil)
		r.Header.Set("Authorization", "Bearer "+adminTestKey)
		adminTestResponse(t, f.server.Router(), r, 400)
		if len(f.store.detailReads) != 0 {
			t.Fatal("invalid ID reached metadata storage")
		}
	}
	for _, test := range []struct {
		name   string
		path   string
		status int
	}{
		{"not found", "/api/admin/request-logs/7", 404},
		{"detail error", "/api/admin/request-logs/7", 500},
		{"list error", "/api/admin/request-logs", 500},
		{"settings error", "/api/admin/request-logs", 500},
		{"no execution", "/api/admin/request-logs/7", 200},
		{"unavailable execution", "/api/admin/request-logs/7", 200},
		{"linked execution", "/api/admin/request-logs/7", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newUserRequestTestFixture(t, 1024)
			f.store.detail = model.UserRequestLog{ID: 7, UserRequestLogInput: model.UserRequestLogInput{RequestID: "exact-entry-id"}}
			if test.name == "not found" {
				f.store.readErr = fmt.Errorf("wrapped lookup: %w", pgx.ErrNoRows)
			} else if test.name == "settings error" {
				f.store.settingsErr = errors.New("private-database-detail-sentinel")
			} else if test.status == 500 {
				f.store.readErr = errors.New("private-database-detail-sentinel")
			} else if test.name != "no execution" {
				requestID := "selected-exact-id"
				f.store.detail.ExecutionRequestID = &requestID
				if test.name == "linked execution" {
					id := int64(12345)
					f.store.executionID = &id
				}
			}
			r := adminTestRequest("GET", test.path, "", nil)
			r.Header.Set("Authorization", "Bearer "+adminTestKey)
			response := adminTestResponse(t, f.server.Router(), r, test.status)
			if strings.Contains(response.Body.String(), "private-database-detail-sentinel") {
				t.Fatal("new admin read exposed raw database failure details")
			}
			if test.status == 200 {
				var body struct {
					Log         model.UserRequestLog `json:"log"`
					ExecutionID *int64               `json:"execution_log_id"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Log.ID != 7 || !reflect.DeepEqual(body.ExecutionID, f.store.executionID) || !reflect.DeepEqual(body.Log.ExecutionRequestID, f.store.detail.ExecutionRequestID) || !bytes.Contains(response.Body.Bytes(), []byte(`"http_status":null`)) {
					t.Fatal("detail lost its exact scalar link or explicit nullable metadata")
				}
				if !reflect.DeepEqual(f.store.detailReads, []int64{7}) || !bytes.Contains(response.Body.Bytes(), []byte(`"execution_log_id":`)) {
					t.Fatal("detail did not use one numeric entry lookup and a nullable link")
				}
			}
		})
	}
}

type userRequestExtractFailureProvider struct {
	policyTestProvider
	err error
}

// Extract returns a systemic synthetic failure, or an ordinary per-URL failure
// when err is nil, to preserve the distinct HTTP versus MCP tool-error mappings.
func (p *userRequestExtractFailureProvider) Extract(_ context.Context, req model.ExtractRequest, _ model.APIKey) (model.ExtractProviderResponse, error) {
	if p.err != nil {
		return model.ExtractProviderResponse{}, p.err
	}
	return model.ExtractProviderResponse{FailedResults: []model.ExtractFailure{{URL: req.URLs[0], Error: "private-extract-failure-sentinel"}}}, nil
}

// TestUserRequestExtractOutcomes preserves systemic provider status mappings and
// ordinary all-failed MCP results while retaining one exact execution per entry.
func TestUserRequestExtractOutcomes(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{"all URLs failed", nil, 200},
		{"upstream", &provider.Error{Type: provider.ErrorTypeUpstream, Message: "private-extract-failure-sentinel"}, 502},
		{"rate limit", &provider.Error{Type: provider.ErrorTypeRateLimited, Message: "private-extract-failure-sentinel"}, 429},
		{"no key", &provider.Error{Type: provider.ErrorTypeNoKey, Message: "private-extract-failure-sentinel"}, 503},
		{"deadline", context.DeadlineExceeded, 504},
	} {
		for _, path := range []string{"/v1/extract", "/v1/compat/tavily/extract", "/custom/mcp"} {
			t.Run(path+"/"+test.name, func(t *testing.T) {
				f := newUserRequestTestFixture(t, 1024)
				adapter := &userRequestExtractFailureProvider{policyTestProvider: policyTestProvider{name: model.ProviderTavily}, err: test.err}
				f.h.orchestrator = search.NewOrchestrator(provider.NewRegistry(adapter), f.pool, f.store)
				body := `{"urls":["https://example.com"],"providers":["tavily"]}`
				status := test.status
				if path == "/custom/mcp" {
					body = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"extract","arguments":` + body + `}}`
					status = 200
				}
				r := adminTestRequest("POST", path, body, nil)
				r.Header.Set("Authorization", "Bearer "+adminTestToken)
				response := adminTestResponse(t, f.server.Router(), r, status)
				entry := onlyUserRequestEntry(t, f, response)
				if len(f.store.logs) != 1 || entry.ExecutionRequestID == nil || *entry.ExecutionRequestID != f.store.logs[0].RequestID || len(f.store.logs[0].Calls) != 1 || f.store.usageMarks != 1 || entry.MCPErrorCount != 0 {
					t.Fatal("Extract provider outcome lost its single execution or changed admission")
				}
				if path == "/custom/mcp" {
					if entry.MCPToolErrorCount != 1 || !bytes.Contains(response.Body.Bytes(), []byte(`"isError":true`)) {
						t.Fatal("HTTP 200 Extract tool failure was mislabeled as success")
					}
				} else if entry.MCPToolErrorCount != 0 {
					t.Fatal("HTTP extraction invented an MCP outcome")
				}
				assertUserRequestPrivacy(t, entry, []string{"private-extract-failure-sentinel"})
			})
		}
	}
}

// TestUserRequestMCPAfterExecution preserves the selected execution through a
// post-provider cancellation, large successful output and response-encoding failure.
func TestUserRequestMCPAfterExecution(t *testing.T) {
	for _, test := range []string{"canceled", "large result", "encoding failure"} {
		t.Run(test, func(t *testing.T) {
			f := newUserRequestTestFixture(t, 4*1024*1024)
			body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"fixture","providers":["brave"]}}}`
			status, completion, protocolErrors := 200, "completed", 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test == "canceled" {
				f.brave.searchHook = cancel
				status, completion, protocolErrors = 408, "canceled", 1
				body = `[{"jsonrpc":"2.0","id":1,"method":"tools/list"},` + body + `,{"jsonrpc":"2.0","id":3,"method":"tools/list"}]`
			} else if test == "large result" {
				f.tavily.content = strings.Repeat("<", 64*1024) + "FULL-CONTENT-TAIL"
				body = `[{"jsonrpc":"2.0","id":1,"method":"tools/list"},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"extract","arguments":{"urls":["https://example.com"],"providers":["tavily"]}}}]`
			} else {
				body = `{"jsonrpc":"2.0","id":"` + strings.Repeat("<", 3*1024*1024) + `","method":"tools/call","params":{"name":"search","arguments":{"query":"fixture","providers":["brave"]}}}`
				status, protocolErrors = 413, 1
			}
			r := adminTestRequest("POST", "/custom/mcp", body, nil).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+adminTestToken)
			response := adminTestResponse(t, f.server.Router(), r, status)
			entry := onlyUserRequestEntry(t, f, response)
			if len(f.store.logs) != 1 || entry.ExecutionRequestID == nil || *entry.ExecutionRequestID != f.store.logs[0].RequestID || entry.Completion != completion || entry.MCPErrorCount != protocolErrors || entry.MCPToolErrorCount != 0 || f.store.usageMarks != 1 {
				t.Fatal("post-execution transport outcome changed accounting or correlation")
			}
			if test == "large result" && (response.Body.Len() <= maxMCPDiscoveryBytes || !bytes.Contains(response.Body.Bytes(), []byte("FULL-CONTENT-TAIL"))) {
				t.Fatal("observer truncated the full Extract output")
			}
			if test == "canceled" && (response.Body.Len() > 256 || bytes.Contains(response.Body.Bytes(), []byte("inputSchema"))) {
				t.Fatal("canceled batch leaked buffered discovery output")
			}
			if test == "encoding failure" {
				assertMCPBoundError(t, response, -32000)
				if response.Body.Len() > 256 {
					t.Fatal("post-execution encoding failure reflected the oversized caller ID")
				}
			}
			assertUserRequestPrivacy(t, entry, []string{"FULL-CONTENT-TAIL", adminTestToken})
		})
	}
}

// TestUserRequestDisplayBounds keeps configured long MCP paths inspectable and
// byte bounded without clipping execution IDs or rewriting response metadata.
func TestUserRequestDisplayBounds(t *testing.T) {
	f := newUserRequestTestFixture(t, 1024)
	path := "/" + strings.Repeat("x", model.UserRequestLogPathMaxBytes+20)
	f.h.EnableMCP(path)
	server := NewServer(config.Config{}, f.log)
	server.Mount(f.h.Mount)
	response := adminTestResponse(t, server.Router(), adminTestRequest("GET", path, "", nil), 200)
	entry := onlyUserRequestEntry(t, f, response)
	if len(entry.Path) != model.UserRequestLogPathMaxBytes || !bytes.Contains(response.Body.Bytes(), []byte(path)) {
		t.Fatal("display-path clipping changed the original MCP endpoint response")
	}
	requestID := strings.Repeat("a", 174)
	ctx := context.WithValue(context.Background(), requestIDKey, requestID)
	r := httptest.NewRequest("POST", "/v1/search", nil).WithContext(ctx)
	input := newUserRequestLogInput(r, "search", model.CompatFormatNative, time.Now())
	state := &userRequestLogState{input: input}
	ctx = context.WithValue(ctx, userRequestLogContextKey{}, state)
	executionID := strings.Repeat("b", model.UserRequestLogIDMaxBytes)
	observeUserRequestExecution(ctx, executionID)
	if input.RequestID != requestID || state.input.ExecutionRequestID == nil || *state.input.ExecutionRequestID != executionID {
		t.Fatal("a valid fallback/correlation identity was clipped")
	}
}
