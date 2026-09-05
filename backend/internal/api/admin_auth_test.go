package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vihor3/searchmeld/backend/internal/config"
	"github.com/vihor3/searchmeld/backend/internal/model"
	"golang.org/x/crypto/bcrypt"
)

const (
	adminTestOrigin   = "http://admin.example:8080"
	adminTestUsername = "operator"
	adminTestPassword = "synthetic-password"
	adminTestKey      = "oak_synthetic-admin-key"
	adminTestToken    = "osr_synthetic-public-token"
)

type adminAuthTestStore struct {
	AppStore
	user         model.AdminUser
	adminKey     string
	adminKeyErr  error
	keyLookups   []string
	tokenLookups []string
	userLookups  []string
	settings     model.RuntimeSettings
	audits       []model.AuditLogInput
	usageMarks   int
}

func (s *adminAuthTestStore) GetAdminByUsername(_ context.Context, username string) (model.AdminUser, error) {
	s.userLookups = append(s.userLookups, username)
	if username != s.user.Username {
		return model.AdminUser{}, errors.New("admin not found")
	}
	return s.user, nil
}

func (s *adminAuthTestStore) FindAdminAPIKey(_ context.Context, token string) (model.AdminAPIKey, bool, error) {
	s.keyLookups = append(s.keyLookups, token)
	if s.adminKeyErr != nil {
		return model.AdminAPIKey{}, false, s.adminKeyErr
	}
	if token == "" || token != s.adminKey {
		return model.AdminAPIKey{}, false, nil
	}
	return model.AdminAPIKey{KeyPrefix: "oak_synthetic"}, true, nil
}

func (s *adminAuthTestStore) FindAPIToken(_ context.Context, token string) (model.APIToken, error) {
	s.tokenLookups = append(s.tokenLookups, token)
	if token != adminTestToken {
		return model.APIToken{}, errors.New("invalid api token")
	}
	return model.APIToken{ID: 42, Scopes: []string{"search", "extract"}, Status: "active"}, nil
}

func (s *adminAuthTestStore) MarkAPITokenUsed(context.Context, int64) error {
	s.usageMarks++
	return nil
}

func (s *adminAuthTestStore) RuntimeSettings(context.Context) (model.RuntimeSettings, error) {
	return s.settings, nil
}

func (s *adminAuthTestStore) RecordAuditLog(_ context.Context, input model.AuditLogInput) error {
	s.audits = append(s.audits, input)
	return nil
}

func (*adminAuthTestStore) ListProviders(context.Context) ([]model.ProviderConfig, error) {
	return []model.ProviderConfig{}, nil
}

func (*adminAuthTestStore) GetAPIKeyByID(_ context.Context, id int64) (model.APIKey, error) {
	return model.APIKey{ID: id, Value: "synthetic-provider-secret"}, nil
}

func (*adminAuthTestStore) RevealAPIToken(_ context.Context, id int64) (model.APIToken, error) {
	return model.APIToken{ID: id, Token: adminTestToken}, nil
}

func (s *adminAuthTestStore) GetAdminAPIKey(context.Context) (model.AdminAPIKey, error) {
	return model.AdminAPIKey{Key: s.adminKey, KeyPrefix: "oak_synthetic"}, nil
}

func (s *adminAuthTestStore) RotateAdminAPIKey(context.Context) (model.AdminAPIKey, string, error) {
	s.adminKey = "oak_synthetic-rotated-key"
	return model.AdminAPIKey{Key: s.adminKey, KeyPrefix: "oak_synthetic"}, s.adminKey, nil
}

type adminAuthTestLogger struct {
	fields []map[string]interface{}
}

func (l *adminAuthTestLogger) Info(_ string, fields map[string]interface{}) {
	l.fields = append(l.fields, fields)
}

func (l *adminAuthTestLogger) Error(_ string, fields map[string]interface{}) {
	l.fields = append(l.fields, fields)
}

type adminAuthFixture struct {
	store  *adminAuthTestStore
	auth   *AuthService
	h      *Handler
	server *Server
	log    *adminAuthTestLogger
}

func newAdminAuthFixture(t *testing.T, cfg config.Config) *adminAuthFixture {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(adminTestPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("generate synthetic password hash: %v", err)
	}
	store := &adminAuthTestStore{
		user:     model.AdminUser{Username: adminTestUsername, PasswordHash: string(hash)},
		adminKey: adminTestKey,
		settings: model.RuntimeSettings{APIAuthRequired: true, CompatTavilyEnabled: true},
	}
	auth := NewAuthService(store, cfg.AdminSessionTTL, cfg.AdminLoginMaxAttempts, cfg.AdminLoginWindow, cfg.AdminLoginLockout)
	h := NewHandler(store, auth, nil)
	h.EnableMCP("/mcp")
	log := &adminAuthTestLogger{}
	server := NewServer(cfg, log)
	server.Mount(h.Mount)
	return &adminAuthFixture{store: store, auth: auth, h: h, server: server, log: log}
}

func adminTestRequest(method, path, body string, cookie *http.Cookie) *http.Request {
	r := httptest.NewRequest(method, adminTestOrigin+path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.10:54321"
	r.Header.Set(adminBrowserHeader, "1")
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return r
}

func adminTestResponse(t *testing.T, handler http.Handler, r *http.Request, wantStatus int) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, r)
	if response.Code != wantStatus {
		t.Fatalf("%s %s status=%d, want %d; body=%s", r.Method, r.URL.Path, response.Code, wantStatus, response.Body.String())
	}
	if isAdminPath(r.URL.Path) {
		if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
			t.Fatalf("admin response is cacheable: %v", response.Header())
		}
		if wantStatus != http.StatusOK || r.URL.Path != "/api/admin/login" {
			if len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("non-login response must not replace or clear a cookie")
			}
		}
	}
	return response
}

func (f *adminAuthFixture) login(t *testing.T) *http.Cookie {
	t.Helper()
	r := adminTestRequest(http.MethodPost, "/api/admin/login", `{"username":"operator","password":"synthetic-password"}`, nil)
	response := adminTestResponse(t, f.server.Router(), r, http.StatusOK)
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != adminSessionCookieName || cookies[0].Value == "" {
		t.Fatal("login must issue exactly one nonempty admin session cookie")
	}
	return cookies[0]
}

func TestAdminLoginCookieContract(t *testing.T) {
	f := newAdminAuthFixture(t, config.Config{AdminSessionTTL: 2 * time.Hour})
	r := adminTestRequest(http.MethodPost, "/api/admin/login", `{"username":"operator","password":"synthetic-password"}`, nil)
	r.Header.Set("Origin", adminTestOrigin)
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	before := time.Now()
	response := adminTestResponse(t, f.server.Router(), r, http.StatusOK)
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if len(body) != 1 || body["expires_at"] == nil {
		t.Fatalf("login response must contain only expires_at; fields=%v", body)
	}
	var expiresAt time.Time
	if err := json.Unmarshal(body["expires_at"], &expiresAt); err != nil {
		t.Fatalf("decode expires_at: %v", err)
	}
	if expiresAt.Before(before.Add(2*time.Hour)) || expiresAt.After(time.Now().Add(2*time.Hour)) {
		t.Fatalf("unexpected login TTL: %v", expiresAt)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookie count = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != adminSessionCookieName || !strings.HasPrefix(cookie.Value, "adm_") || cookie.Path != "/api/admin" ||
		cookie.Domain != "" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Secure ||
		cookie.MaxAge != 0 || !cookie.Expires.IsZero() {
		t.Fatal("login cookie does not match the host-only HTTP browser-session contract")
	}
	if !f.auth.sessions[cookie.Value].ExpiresAt.Equal(expiresAt) {
		t.Fatal("login metadata does not match the fixed server expiry")
	}
	me := adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodGet, "/api/admin/me", "", cookie), http.StatusOK)
	if strings.TrimSpace(me.Body.String()) != `{"username":"admin"}` {
		t.Fatalf("me compatibility response = %s", me.Body.String())
	}
	logged, err := json.Marshal([]interface{}{f.store.audits, f.log.fields})
	if err != nil {
		t.Fatalf("serialize recorded logs: %v", err)
	}
	for _, secret := range []string{cookie.Value, adminTestPassword, f.store.user.PasswordHash} {
		if strings.Contains(string(logged), secret) || strings.Contains(response.Body.String(), secret) || strings.Contains(me.Body.String(), secret) {
			t.Fatal("session/password secret leaked into response JSON or logs")
		}
	}
	if len(f.store.audits) != 1 || f.store.audits[0].Action != "admin.login" {
		t.Fatalf("login audits = %+v", f.store.audits)
	}
	second := f.login(t)
	if second.Value == cookie.Value {
		t.Fatal("each login must create a new opaque session")
	}
}

func TestAdminLoginRejectsInvalidRequests(t *testing.T) {
	validBody := `{"username":"operator","password":"synthetic-password"}`
	for _, test := range []struct {
		name       string
		body       string
		media      string
		proof      string
		origin     string
		wantStatus int
	}{
		{name: "wrong password", body: `{"username":"operator","password":"wrong"}`, media: "application/json", proof: "1", wantStatus: http.StatusUnauthorized},
		{name: "unknown user", body: `{"username":"missing","password":"synthetic-password"}`, media: "application/json", proof: "1", wantStatus: http.StatusUnauthorized},
		{name: "malformed JSON", body: `{`, media: "application/json", proof: "1", wantStatus: http.StatusBadRequest},
		{name: "trailing JSON", body: validBody + `{}`, media: "application/json", proof: "1", wantStatus: http.StatusBadRequest},
		{name: "trailing garbage", body: validBody + `x`, media: "application/json", proof: "1", wantStatus: http.StatusBadRequest},
		{name: "wrong field type", body: `{"username":1}`, media: "application/json", proof: "1", wantStatus: http.StatusBadRequest},
		{name: "missing media type", body: validBody, proof: "1", wantStatus: http.StatusUnsupportedMediaType},
		{name: "form media type", body: validBody, media: "application/x-www-form-urlencoded", proof: "1", wantStatus: http.StatusUnsupportedMediaType},
		{name: "plain text", body: validBody, media: "text/plain", proof: "1", wantStatus: http.StatusUnsupportedMediaType},
		{name: "invalid media parameter", body: validBody, media: "application/json; charset=", proof: "1", wantStatus: http.StatusUnsupportedMediaType},
		{name: "missing proof", body: validBody, media: "application/json", wantStatus: http.StatusForbidden},
		{name: "wrong proof", body: validBody, media: "application/json", proof: "true", wantStatus: http.StatusForbidden},
		{name: "untrusted origin", body: validBody, media: "application/json", proof: "1", origin: "http://hostile.example:8080", wantStatus: http.StatusForbidden},
		{name: "null origin", body: validBody, media: "application/json", proof: "1", origin: "null", wantStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAdminAuthFixture(t, config.Config{CorsOrigins: []string{"*"}})
			r := adminTestRequest(http.MethodPost, "/api/admin/login", test.body, nil)
			r.Header.Set("Content-Type", test.media)
			r.Header.Set(adminBrowserHeader, test.proof)
			if test.origin != "" {
				r.Header.Set("Origin", test.origin)
			}
			response := adminTestResponse(t, f.server.Router(), r, test.wantStatus)
			var body struct {
				Error struct {
					Message string `json:"message"`
					Status  int    `json:"status"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Status != test.wantStatus || body.Error.Message == "" {
				t.Fatalf("invalid native error envelope: %s", response.Body.String())
			}
			if len(f.auth.sessions) != 0 {
				t.Fatal("rejected login created a session")
			}
			if test.wantStatus != http.StatusUnauthorized && len(f.store.audits) != 0 {
				t.Fatal("rejected browser/JSON request reached password authentication")
			}
		})
	}
}

func TestAdminHeaderCredentialPrecedence(t *testing.T) {
	for _, test := range []struct {
		name          string
		authorization []string
		apiKey        []string
		cookie        string
		proof         bool
		storeError    bool
		wantStatus    int
		wantLookup    string
	}{
		{name: "cookie", cookie: "valid", proof: true, wantStatus: http.StatusOK},
		{name: "cookie without proof", cookie: "valid", wantStatus: http.StatusForbidden},
		{name: "no credentials", proof: true, wantStatus: http.StatusUnauthorized},
		{name: "unknown cookie", cookie: "invalid", proof: true, wantStatus: http.StatusUnauthorized},
		{name: "key in cookie", cookie: adminTestKey, proof: true, wantStatus: http.StatusUnauthorized},
		{name: "bearer key no proof", authorization: []string{"Bearer " + adminTestKey}, wantStatus: http.StatusOK, wantLookup: adminTestKey},
		{name: "x-api-key no proof", apiKey: []string{adminTestKey}, wantStatus: http.StatusOK, wantLookup: adminTestKey},
		{name: "key with stale cookie", authorization: []string{"Bearer " + adminTestKey}, cookie: "invalid", wantStatus: http.StatusOK, wantLookup: adminTestKey},
		{name: "key with incidental cookie", apiKey: []string{adminTestKey}, cookie: "valid", wantStatus: http.StatusOK, wantLookup: adminTestKey},
		{name: "case insensitive bearer", authorization: []string{"bEaReR  " + adminTestKey + " "}, wantStatus: http.StatusOK, wantLookup: adminTestKey},
		{name: "bearer wins", authorization: []string{"Bearer " + adminTestKey}, apiKey: []string{"oak_invalid"}, wantStatus: http.StatusOK, wantLookup: adminTestKey},
		{name: "invalid bearer suppresses key", authorization: []string{"Bearer oak_invalid"}, apiKey: []string{adminTestKey}, cookie: "valid", proof: true, wantStatus: http.StatusUnauthorized, wantLookup: "oak_invalid"},
		{name: "empty bearer suppresses key", authorization: []string{"Bearer "}, apiKey: []string{adminTestKey}, cookie: "valid", proof: true, wantStatus: http.StatusUnauthorized},
		{name: "other scheme retains x-api-key precedence", authorization: []string{"Basic ignored"}, apiKey: []string{adminTestKey}, wantStatus: http.StatusOK, wantLookup: adminTestKey},
		{name: "empty authorization", authorization: []string{""}, cookie: "valid", proof: true, wantStatus: http.StatusUnauthorized},
		{name: "empty x-api-key", apiKey: []string{" "}, cookie: "valid", proof: true, wantStatus: http.StatusUnauthorized},
		{name: "malformed scheme", authorization: []string{"Basic ignored"}, cookie: "valid", proof: true, wantStatus: http.StatusUnauthorized},
		{name: "legacy bearer session", authorization: []string{"session"}, cookie: "valid", proof: true, wantStatus: http.StatusUnauthorized},
		{name: "legacy x-api-key session", apiKey: []string{"session"}, cookie: "valid", proof: true, wantStatus: http.StatusUnauthorized},
		{name: "ordinary bearer token", authorization: []string{"Bearer " + adminTestToken}, cookie: "valid", proof: true, wantStatus: http.StatusUnauthorized},
		{name: "ordinary x-api-key token", apiKey: []string{adminTestToken}, cookie: "valid", proof: true, wantStatus: http.StatusUnauthorized},
		{name: "prefix is not proof", apiKey: []string{"oak_invalid"}, cookie: "valid", wantStatus: http.StatusUnauthorized, wantLookup: "oak_invalid"},
		{name: "duplicate bearer", authorization: []string{"Bearer " + adminTestKey, "Bearer " + adminTestKey}, cookie: "valid", proof: true, wantStatus: http.StatusUnauthorized},
		{name: "duplicate api key", apiKey: []string{adminTestKey, adminTestKey}, cookie: "valid", proof: true, wantStatus: http.StatusUnauthorized},
		{name: "key storage failure", authorization: []string{"Bearer " + adminTestKey}, cookie: "valid", proof: true, storeError: true, wantStatus: http.StatusInternalServerError, wantLookup: adminTestKey},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAdminAuthFixture(t, config.Config{})
			valid := f.login(t)
			var cookie *http.Cookie
			if test.cookie == "valid" {
				cookie = valid
			} else if test.cookie != "" {
				cookie = &http.Cookie{Name: adminSessionCookieName, Value: test.cookie}
			}
			r := adminTestRequest(http.MethodGet, "/api/admin/me", "", cookie)
			if !test.proof {
				r.Header.Del(adminBrowserHeader)
			}
			if test.authorization != nil {
				r.Header["Authorization"] = append([]string(nil), test.authorization...)
				if test.authorization[0] == "session" {
					r.Header.Set("Authorization", "Bearer "+valid.Value)
				}
			}
			if test.apiKey != nil {
				r.Header[http.CanonicalHeaderKey("X-API-Key")] = append([]string(nil), test.apiKey...)
				if test.apiKey[0] == "session" {
					r.Header.Set("X-API-Key", valid.Value)
				}
			}
			if test.storeError {
				f.store.adminKeyErr = errors.New("storage failure containing " + adminTestKey)
			}
			response := adminTestResponse(t, f.server.Router(), r, test.wantStatus)
			if strings.Contains(response.Body.String(), adminTestKey) || strings.Contains(response.Body.String(), valid.Value) {
				t.Fatal("credential leaked in admin response")
			}
			if len(f.store.tokenLookups) != 0 {
				t.Fatal("admin authentication consulted ordinary API tokens")
			}
			if test.wantLookup == "" {
				if len(f.store.keyLookups) != 0 {
					t.Fatal("unexpected admin key lookup")
				}
			} else if len(f.store.keyLookups) != 1 || f.store.keyLookups[0] != test.wantLookup {
				t.Fatal("admin key lookup did not use the selected exact credential")
			}
			if !f.auth.validSession(valid.Value) {
				t.Fatal("credential/proof rejection revoked an incidental session")
			}
		})
	}
}

func TestAdminCookieTTLRevocationAndRestart(t *testing.T) {
	f := newAdminAuthFixture(t, config.Config{AdminSessionTTL: time.Hour})
	cookie := f.login(t)
	expiresAt := f.auth.sessions[cookie.Value].ExpiresAt
	for i := 0; i < 2; i++ {
		adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodGet, "/api/admin/me", "", cookie), http.StatusOK)
		if !f.auth.sessions[cookie.Value].ExpiresAt.Equal(expiresAt) {
			t.Fatal("protected access extended the fixed session TTL")
		}
	}
	adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodGet, "/api/admin/me", "", nil), http.StatusUnauthorized)
	adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodPost, "/api/admin/logout", "", cookie), http.StatusOK)
	if f.auth.validSession(cookie.Value) {
		t.Fatal("logout did not revoke the authenticated session")
	}
	for _, path := range []string{"/api/admin/me", "/api/admin/logout"} {
		method := http.MethodGet
		if strings.HasSuffix(path, "logout") {
			method = http.MethodPost
		}
		adminTestResponse(t, f.server.Router(), adminTestRequest(method, path, "", cookie), http.StatusUnauthorized)
	}
	expired := f.login(t)
	f.auth.mu.Lock()
	s := f.auth.sessions[expired.Value]
	s.ExpiresAt = time.Now().Add(-time.Second)
	f.auth.sessions[expired.Value] = s
	f.auth.mu.Unlock()
	adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodGet, "/api/admin/me", "", expired), http.StatusUnauthorized)
	if _, ok := f.auth.sessions[expired.Value]; ok {
		t.Fatal("expired session was not removed")
	}
	beforeRestart := f.login(t)
	f.auth = NewAuthService(f.store, time.Hour, 0, 0, 0)
	f.h.auth = f.auth
	restarted := NewServer(config.Config{}, f.log)
	restarted.Mount(f.h.Mount)
	adminTestResponse(t, restarted.Router(), adminTestRequest(http.MethodGet, "/api/admin/me", "", beforeRestart), http.StatusUnauthorized)
	r := adminTestRequest(http.MethodGet, "/api/admin/me", "", nil)
	r.Header.Set("Authorization", "Bearer "+adminTestKey)
	r.Header.Del(adminBrowserHeader)
	adminTestResponse(t, restarted.Router(), r, http.StatusOK)
}

func TestAdminLogoutAndKeyRotationAreCredentialScoped(t *testing.T) {
	f := newAdminAuthFixture(t, config.Config{})
	cookie := f.login(t)
	for _, header := range []string{"Authorization", "X-API-Key"} {
		r := adminTestRequest(http.MethodPost, "/api/admin/logout", "", cookie)
		value := adminTestKey
		if header == "Authorization" {
			value = "Bearer " + value
		}
		r.Header.Set(header, value)
		r.Header.Del(adminBrowserHeader)
		response := adminTestResponse(t, f.server.Router(), r, http.StatusOK)
		if strings.TrimSpace(response.Body.String()) != `{"status":"ok"}` || !f.auth.validSession(cookie.Value) || f.store.adminKey != adminTestKey {
			t.Fatal("key logout must preserve both the key and the incidental cookie session")
		}
		audit := f.store.audits[len(f.store.audits)-1]
		if audit.Actor != "admin_api_key:oak_synthetic" || audit.Metadata["logged_out"] != false {
			t.Fatalf("key logout audit = %+v", audit)
		}
	}
	r := adminTestRequest(http.MethodPost, "/api/admin/logout", "", cookie)
	r.Header.Set("Authorization", "Bearer oak_invalid")
	adminTestResponse(t, f.server.Router(), r, http.StatusUnauthorized)
	if !f.auth.validSession(cookie.Value) {
		t.Fatal("invalid header logout revoked the cookie session")
	}
	r = adminTestRequest(http.MethodPost, "/api/admin/settings/admin-api-key", "", cookie)
	adminTestResponse(t, f.server.Router(), r, http.StatusCreated)
	if f.store.adminKey == adminTestKey || !f.auth.validSession(cookie.Value) {
		t.Fatal("key rotation did not preserve the browser session")
	}
	r = adminTestRequest(http.MethodGet, "/api/admin/me", "", cookie)
	r.Header.Set("X-API-Key", adminTestKey)
	adminTestResponse(t, f.server.Router(), r, http.StatusUnauthorized)
	r.Header.Set("X-API-Key", f.store.adminKey)
	adminTestResponse(t, f.server.Router(), r, http.StatusOK)
	adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodPost, "/api/admin/logout", "", cookie), http.StatusOK)
	r = adminTestRequest(http.MethodGet, "/api/admin/me", "", nil)
	r.Header.Set("X-API-Key", f.store.adminKey)
	adminTestResponse(t, f.server.Router(), r, http.StatusOK)
}

func TestAdminFirstKeySetupStillUsesPasswordSession(t *testing.T) {
	f := newAdminAuthFixture(t, config.Config{})
	f.store.adminKey = ""
	cookie := f.login(t)
	r := adminTestRequest(http.MethodPost, "/api/admin/settings/admin-api-key", "", cookie)
	response := adminTestResponse(t, f.server.Router(), r, http.StatusCreated)
	var key model.AdminAPIKey
	if err := json.Unmarshal(response.Body.Bytes(), &key); err != nil || key.Key == "" || key.Key != f.store.adminKey {
		t.Fatal("password-session first-key setup lost its explicit programmatic secret response")
	}
	r = adminTestRequest(http.MethodGet, "/api/admin/me", "", nil)
	r.Header.Set("Authorization", "Bearer "+key.Key)
	r.Header.Del(adminBrowserHeader)
	adminTestResponse(t, f.server.Router(), r, http.StatusOK)
}

func TestAdminDelayedLogoutCannotRevokeNewSession(t *testing.T) {
	f := newAdminAuthFixture(t, config.Config{})
	oldCookie := f.login(t)
	var newCookie *http.Cookie
	heldServer := NewServer(config.Config{}, f.log)
	heldServer.Mount(func(r chi.Router) {
		r.With(f.auth.requireAdmin).Post("/api/admin/logout", func(w http.ResponseWriter, r *http.Request) {
			credential, ok := r.Context().Value(adminCredentialKey).(adminCredential)
			if !ok || credential.Kind != adminCookieCredential || credential.SessionToken != oldCookie.Value {
				t.Fatal("middleware did not capture the authenticated session")
			}
			// Complete a newer login after this logout was authenticated but before it responds.
			newCookie = f.login(t)
			f.h.logout(w, r)
		})
	})
	adminTestResponse(t, heldServer.Router(), adminTestRequest(http.MethodPost, "/api/admin/logout", "", oldCookie), http.StatusOK)
	if newCookie == nil {
		t.Fatal("newer login did not complete before the old logout response")
	}
	if f.auth.validSession(oldCookie.Value) || !f.auth.validSession(newCookie.Value) {
		t.Fatal("delayed logout did not revoke only its captured session")
	}
	adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodGet, "/api/admin/me", "", oldCookie), http.StatusUnauthorized)
	adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodGet, "/api/admin/me", "", newCookie), http.StatusOK)
	adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodPost, "/api/admin/logout", "", oldCookie), http.StatusUnauthorized)
	adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodGet, "/api/admin/me", "", newCookie), http.StatusOK)
}

func TestAdminNoStoreIncludesSecretsAndRoutingErrors(t *testing.T) {
	f := newAdminAuthFixture(t, config.Config{})
	cookie := f.login(t)
	for _, path := range []string{"/api/admin/keys/1/secret", "/api/admin/tokens/1/secret", "/api/admin/settings/admin-api-key"} {
		r := adminTestRequest(http.MethodGet, path, "", cookie)
		response := adminTestResponse(t, f.server.Router(), r, http.StatusOK)
		if !strings.Contains(response.Body.String(), "synthetic") {
			t.Fatal("secret response did not reach the real mounted reveal handler")
		}
	}
	adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodGet, "/api/admin/missing", "", cookie), http.StatusNotFound)
	adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodDelete, "/api/admin/me", "", cookie), http.StatusMethodNotAllowed)
	adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodGet, "/api/admin", "", cookie), http.StatusNotFound)
}

func TestAdminLoginLockoutUsesTrustedPeerIP(t *testing.T) {
	for _, test := range []struct {
		name       string
		peer       string
		trustedIP  string
		wantIP     string
		wantRemote string
	}{
		{name: "direct remote", peer: "192.0.2.10:54321", wantIP: "192.0.2.10", wantRemote: "192.0.2.10:54321"},
		{name: "packaged loopback proxy", peer: "127.0.0.1:54321", trustedIP: "192.0.2.10", wantIP: "192.0.2.10", wantRemote: "192.0.2.10"},
		{name: "IPv6 loopback proxy", peer: "[::1]:54321", trustedIP: "2001:db8::10", wantIP: "2001:db8::10", wantRemote: "2001:db8::10"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAdminAuthFixture(t, config.Config{AdminLoginMaxAttempts: 3})
			for attempt := 1; attempt <= 5; attempt++ {
				password := "wrong"
				if attempt == 5 {
					password = adminTestPassword
				}
				body, err := json.Marshal(map[string]string{"username": adminTestUsername, "password": password})
				if err != nil {
					t.Fatalf("encode synthetic login: %v", err)
				}
				r := adminTestRequest(http.MethodPost, "/api/admin/login", string(body), nil)
				r.RemoteAddr = test.peer
				spoofed := fmt.Sprintf("198.51.100.%d", attempt)
				r.Header.Set("True-Client-IP", spoofed)
				r.Header.Set("X-Forwarded-For", spoofed+", 203.0.113.1")
				r.Header.Set("X-Real-IP", spoofed)
				if test.trustedIP != "" {
					r.Header.Set("X-Real-IP", test.trustedIP)
				}
				wantStatus := http.StatusUnauthorized
				if attempt >= 3 {
					wantStatus = http.StatusTooManyRequests
				}
				adminTestResponse(t, f.server.Router(), r, wantStatus)
				audit := f.store.audits[len(f.store.audits)-1]
				if audit.Metadata["ip"] != test.wantIP || audit.IPAddress != test.wantRemote {
					t.Fatalf("forged headers changed login/audit IP: %+v", audit)
				}
			}
			key := loginAttemptKey(adminTestUsername, test.wantIP)
			if len(f.auth.loginWindows) != 1 || !f.auth.loginLocked(key) || len(f.auth.sessions) != 0 {
				t.Fatal("forwarded-header variation escaped the socket/proxy login lockout bucket")
			}
			f.auth.mu.Lock()
			window := f.auth.loginWindows[key]
			window.LockedUntil = time.Now().Add(-time.Second)
			f.auth.loginWindows[key] = window
			f.auth.mu.Unlock()
			r := adminTestRequest(http.MethodPost, "/api/admin/login", `{"username":"operator","password":"synthetic-password"}`, nil)
			r.RemoteAddr = test.peer
			if test.trustedIP != "" {
				r.Header.Set("X-Real-IP", test.trustedIP)
			}
			adminTestResponse(t, f.server.Router(), r, http.StatusOK)
			if len(f.auth.loginWindows) != 0 {
				t.Fatal("successful login after lockout expiry did not clear its bucket")
			}
		})
	}
}

func TestBrowserCredentialsDoNotAuthenticatePublicRoutes(t *testing.T) {
	f := newAdminAuthFixture(t, config.Config{})
	cookie := f.login(t)
	for _, path := range []string{
		"/v1/search", "/v1/extract", "/v1/compat/tavily/search", "/v1/compat/tavily/extract",
		"/v1/compat/serper/search", "/v1/compat/openai/responses-search",
		"/mcp", "/mcp/", "/v1/mcp", "/v1/mcp/",
	} {
		for _, credential := range []string{"cookie only", "session bearer", "session api key", "session body key"} {
			t.Run(path+"/"+credential, func(t *testing.T) {
				method := http.MethodPost
				body := `{}`
				mcp := strings.Contains(path, "mcp")
				if mcp {
					body = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{}}}`
				}
				if credential == "session body key" && !mcp {
					payload, err := json.Marshal(map[string]string{"api_key": cookie.Value})
					if err != nil {
						t.Fatalf("encode body credential: %v", err)
					}
					body = string(payload)
				}
				r := adminTestRequest(method, path, body, cookie)
				if credential == "session bearer" {
					r.Header.Set("Authorization", "Bearer "+cookie.Value)
				} else if credential == "session api key" {
					r.Header.Set("X-API-Key", cookie.Value)
				}
				response := adminTestResponse(t, f.server.Router(), r, http.StatusUnauthorized)
				if mcp {
					var result mcpResponse
					if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Error == nil || result.Error.Code != -32001 {
						t.Fatalf("MCP auth rejection envelope = %s", response.Body.String())
					}
				}
				if f.store.usageMarks != 0 {
					t.Fatal("browser-only credential was counted as a public API token")
				}
			})
		}
	}
}

func TestPublicProgrammaticCredentialsRemainIndependent(t *testing.T) {
	f := newAdminAuthFixture(t, config.Config{})
	cookie := f.login(t)
	for _, token := range []string{adminTestKey, adminTestToken} {
		for _, header := range []string{"Authorization", "X-API-Key"} {
			for _, path := range []string{"/v1/search", "/mcp"} {
				r := adminTestRequest(http.MethodPost, path, `{}`, cookie)
				wantStatus := http.StatusBadRequest
				if path == "/mcp" {
					r = adminTestRequest(http.MethodPost, path, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{}}}`, cookie)
					wantStatus = http.StatusOK
				}
				r.Header.Del(adminBrowserHeader)
				value := token
				if header == "Authorization" {
					value = "Bearer " + value
				}
				r.Header.Set(header, value)
				before := f.store.usageMarks
				response := adminTestResponse(t, f.server.Router(), r, wantStatus)
				if !strings.Contains(response.Body.String(), "query is required") {
					t.Fatal("programmatic credential did not reach search validation")
				}
				wantMarks := before
				if token == adminTestToken {
					wantMarks++
				}
				if f.store.usageMarks != wantMarks {
					t.Fatal("programmatic credential accounting changed")
				}
			}
		}
	}
	f.store.settings.APIAuthRequired = false
	adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodPost, "/v1/search", `{}`, nil), http.StatusBadRequest)
	f.store.settings.APIAuthRequired = true
	r := adminTestRequest(http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	adminTestResponse(t, f.server.Router(), r, http.StatusOK)
}
