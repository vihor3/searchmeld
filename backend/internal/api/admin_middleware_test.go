package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/vihor3/searchmeld/backend/internal/config"
)

func TestAdminBrowserOriginBoundary(t *testing.T) {
	for _, test := range []struct {
		name       string
		origins    []string
		wantStatus int
	}{
		{name: "originless with proof", wantStatus: http.StatusOK},
		{name: "canonical origin", origins: []string{adminTestOrigin}, wantStatus: http.StatusOK},
		{name: "canonical host case", origins: []string{"http://ADMIN.EXAMPLE:8080"}, wantStatus: http.StatusOK},
		{name: "explicit trusted origin", origins: []string{"http://ui.admin.example:5173"}, wantStatus: http.StatusOK},
		{name: "same-site hostile origin", origins: []string{"http://hostile.admin.example:8080"}, wantStatus: http.StatusForbidden},
		{name: "wildcard not trusted", origins: []string{"https://arbitrary.example"}, wantStatus: http.StatusForbidden},
		{name: "invalid allowlist entry ignored", origins: []string{"http://hostile.example"}, wantStatus: http.StatusForbidden},
		{name: "wrong port", origins: []string{"http://admin.example:8081"}, wantStatus: http.StatusForbidden},
		{name: "wrong scheme", origins: []string{"https://admin.example:8080"}, wantStatus: http.StatusForbidden},
		{name: "null", origins: []string{"null"}, wantStatus: http.StatusForbidden},
		{name: "empty supplied", origins: []string{""}, wantStatus: http.StatusForbidden},
		{name: "duplicate", origins: []string{adminTestOrigin, adminTestOrigin}, wantStatus: http.StatusForbidden},
		{name: "multiple origins", origins: []string{adminTestOrigin + " https://hostile.example"}, wantStatus: http.StatusForbidden},
		{name: "comma separated origins", origins: []string{adminTestOrigin + ",https://hostile.example"}, wantStatus: http.StatusForbidden},
		{name: "malformed", origins: []string{"://"}, wantStatus: http.StatusForbidden},
		{name: "path", origins: []string{adminTestOrigin + "/page"}, wantStatus: http.StatusForbidden},
		{name: "trailing slash", origins: []string{adminTestOrigin + "/"}, wantStatus: http.StatusForbidden},
		{name: "empty query", origins: []string{adminTestOrigin + "?"}, wantStatus: http.StatusForbidden},
		{name: "empty fragment", origins: []string{adminTestOrigin + "#"}, wantStatus: http.StatusForbidden},
		{name: "userinfo", origins: []string{"http://user@admin.example:8080"}, wantStatus: http.StatusForbidden},
		{name: "leading whitespace", origins: []string{" " + adminTestOrigin}, wantStatus: http.StatusForbidden},
	} {
		for _, mode := range []string{"cookie read", "cookie write", "admin key"} {
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				f := newAdminAuthFixture(t, config.Config{CorsOrigins: []string{
					"*", "http://ui.admin.example:5173/", "http://hostile.example/path", "null", "https://*.example",
				}})
				cookie := f.login(t)
				method, path := http.MethodGet, "/api/admin/me"
				if mode == "cookie write" {
					method, path = http.MethodPost, "/api/admin/logout"
				}
				r := adminTestRequest(method, path, "", cookie)
				if test.origins != nil {
					r.Header["Origin"] = test.origins
				}
				if mode == "admin key" {
					r.Header.Set("Authorization", "Bearer "+adminTestKey)
					r.Header.Del(adminBrowserHeader)
				}
				response := adminTestResponse(t, f.server.Router(), r, test.wantStatus)
				wantOrigin := ""
				if test.wantStatus == http.StatusOK && len(test.origins) == 1 {
					wantOrigin = test.origins[0]
				}
				if response.Header().Get("Access-Control-Allow-Origin") != wantOrigin {
					t.Fatalf("admin CORS origin = %q, want %q", response.Header().Get("Access-Control-Allow-Origin"), wantOrigin)
				}
				if !strings.Contains(strings.Join(response.Header().Values("Vary"), ","), "Origin") {
					t.Fatal("admin response is missing Vary: Origin")
				}
				if test.wantStatus == http.StatusForbidden && (len(f.store.keyLookups) != 0 || !f.auth.validSession(cookie.Value)) {
					t.Fatal("untrusted origin reached key authentication or revoked a session")
				}
			})
		}
	}
}

func TestAdminCookieSecureUsesConfiguredOriginOrDirectTLS(t *testing.T) {
	for _, test := range []struct {
		name         string
		publicOrigin string
		target       string
		origin       string
		secure       bool
		wantStatus   int
	}{
		{name: "direct HTTP port", target: "http://admin.example:18080", origin: "http://admin.example:18080", wantStatus: http.StatusOK},
		{name: "direct HTTPS", target: "https://admin.example:8443", origin: "https://admin.example:8443", secure: true, wantStatus: http.StatusOK},
		{name: "HTTPS default port", target: "https://ADMIN.EXAMPLE:443", origin: "https://admin.example", secure: true, wantStatus: http.StatusOK},
		{name: "HTTP default port", target: "http://ADMIN.EXAMPLE:80", origin: "http://admin.example", wantStatus: http.StatusOK},
		{name: "configured HTTPS proxy", publicOrigin: "HTTPS://Admin.Example:443/", target: "http://127.0.0.1:8080", origin: "https://admin.example", secure: true, wantStatus: http.StatusOK},
		{name: "configured HTTPS nondefault port", publicOrigin: "https://admin.example:8443", target: "http://admin.example:8443", origin: "https://admin.example:8443", secure: true, wantStatus: http.StatusOK},
		{name: "configured HTTP cannot downgrade backend TLS", publicOrigin: "http://admin.example:18080", target: "https://internal.example:8443", origin: "http://admin.example:18080", secure: true, wantStatus: http.StatusOK},
		{name: "direct IPv6", target: "http://[::1]:18080", origin: "http://[::1]:18080", wantStatus: http.StatusOK},
		{name: "HTTPS proxy missing configuration", target: "http://admin.example:8443", origin: "https://admin.example:8443", wantStatus: http.StatusForbidden},
		{name: "configured origin suppresses internal host", publicOrigin: "https://admin.example", target: "http://127.0.0.1:8080", origin: "http://127.0.0.1:8080", wantStatus: http.StatusForbidden},
		{name: "invalid configured origin fails closed", publicOrigin: "https://admin.example/path", target: adminTestOrigin, origin: adminTestOrigin, wantStatus: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAdminAuthFixture(t, config.Config{AdminPublicOrigin: test.publicOrigin})
			r := httptest.NewRequest(http.MethodPost, test.target+"/api/admin/login", strings.NewReader(`{"username":"operator","password":"synthetic-password"}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set(adminBrowserHeader, "1")
			r.Header.Set("Origin", test.origin)
			r.Header.Set("Forwarded", `for=198.51.100.1;proto=https;host=hostile.example`)
			r.Header.Set("X-Forwarded-Host", "hostile.example")
			r.Header.Set("X-Forwarded-Proto", "https")
			if test.secure {
				r.Header.Set("X-Forwarded-Proto", "http")
			}
			response := adminTestResponse(t, f.server.Router(), r, test.wantStatus)
			if test.wantStatus != http.StatusOK {
				if len(f.auth.sessions) != 0 {
					t.Fatal("invalid origin issued a server session")
				}
				return
			}
			cookies := response.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Secure != test.secure || cookies[0].Domain != "" {
				t.Fatal("cookie transport policy did not use the configured/direct origin")
			}
		})
	}
}

func TestAdminCORSPreflightIsIsolatedFromPublicCORS(t *testing.T) {
	for _, test := range []struct {
		name       string
		origin     string
		method     string
		headers    string
		wantStatus int
	}{
		{name: "login preflight", origin: adminTestOrigin, method: "POST", headers: "content-type, x-searchmeld-admin", wantStatus: http.StatusNoContent},
		{name: "trusted explicit origin", origin: "http://ui.admin.example:5173", method: "GET", headers: "X-SearchMeld-Admin", wantStatus: http.StatusNoContent},
		{name: "key headers", origin: adminTestOrigin, method: "PATCH", headers: "authorization, x-api-key, x-request-id", wantStatus: http.StatusNoContent},
		{name: "wildcard cannot allow hostile origin", origin: "http://hostile.admin.example", method: "POST", headers: "x-searchmeld-admin", wantStatus: http.StatusForbidden},
		{name: "null origin", origin: "null", method: "POST", headers: "x-searchmeld-admin", wantStatus: http.StatusForbidden},
		{name: "missing origin", method: "POST", headers: "x-searchmeld-admin", wantStatus: http.StatusForbidden},
		{name: "missing request method", origin: adminTestOrigin, headers: "x-searchmeld-admin", wantStatus: http.StatusForbidden},
		{name: "unsupported method", origin: adminTestOrigin, method: "TRACE", headers: "x-searchmeld-admin", wantStatus: http.StatusForbidden},
		{name: "unsupported header", origin: adminTestOrigin, method: "POST", headers: "x-untrusted-header", wantStatus: http.StatusForbidden},
		{name: "empty header list member", origin: adminTestOrigin, method: "POST", headers: "x-searchmeld-admin,", wantStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAdminAuthFixture(t, config.Config{CorsOrigins: []string{"*", "http://ui.admin.example:5173"}})
			r := adminTestRequest(http.MethodOptions, "/api/admin/login", "", nil)
			r.Header.Del(adminBrowserHeader)
			if test.origin != "" {
				r.Header.Set("Origin", test.origin)
			}
			if test.method != "" {
				r.Header.Set("Access-Control-Request-Method", test.method)
			}
			if test.headers != "" {
				r.Header.Set("Access-Control-Request-Headers", test.headers)
			}
			response := adminTestResponse(t, f.server.Router(), r, test.wantStatus)
			if test.wantStatus == http.StatusNoContent {
				if response.Header().Get("Access-Control-Allow-Origin") != test.origin || response.Header().Get("Access-Control-Allow-Credentials") != "true" ||
					!strings.Contains(response.Header().Get("Access-Control-Allow-Headers"), adminBrowserHeader) {
					t.Fatal("trusted admin preflight lost its origin/credential/proof contract")
				}
			}
			if len(f.store.keyLookups) != 0 || len(f.store.audits) != 0 || len(f.auth.sessions) != 0 {
				t.Fatal("preflight reached authentication or created a session")
			}
		})
	}
	f := newAdminAuthFixture(t, config.Config{CorsOrigins: []string{"*"}})
	for _, path := range []string{"/v1/search", "/mcp", "/api/administrator"} {
		r := adminTestRequest(http.MethodOptions, path, "", nil)
		r.Header.Set("Origin", "https://public-client.example")
		r.Header.Set("Access-Control-Request-Method", "POST")
		response := adminTestResponse(t, f.server.Router(), r, http.StatusNoContent)
		if response.Header().Get("Access-Control-Allow-Origin") != "https://public-client.example" || response.Header().Get("Cache-Control") != "" ||
			strings.Contains(response.Header().Get("Access-Control-Allow-Headers"), adminBrowserHeader) {
			t.Fatal("admin CORS/cache policy leaked into public paths")
		}
	}
}

func TestAllowlistedHTTPSOriginRequiresExplicitProxyConfiguration(t *testing.T) {
	const publicOrigin = "https://admin.example:8443"
	for _, configured := range []bool{false, true} {
		cfg := config.Config{CorsOrigins: []string{"*", publicOrigin}}
		if configured {
			cfg.AdminPublicOrigin = publicOrigin
		}
		f := newAdminAuthFixture(t, cfg)
		cookie := f.login(t)
		for _, path := range []string{"/api/admin/login", "/api/admin/me", "/api/admin/logout"} {
			body, method := "", http.MethodGet
			if path == "/api/admin/login" {
				body, method = `{"username":"operator","password":"synthetic-password"}`, http.MethodPost
			} else if path == "/api/admin/logout" {
				method = http.MethodPost
			}
			r := adminTestRequest(method, path, body, cookie)
			r.Host = "admin.example:8443"
			r.Header.Set("Origin", publicOrigin)
			r.Header.Set("X-Forwarded-Proto", "https")
			wantStatus := http.StatusForbidden
			if configured {
				wantStatus = http.StatusOK
			}
			before := len(f.auth.sessions)
			response := adminTestResponse(t, f.server.Router(), r, wantStatus)
			if !configured {
				if !strings.Contains(response.Body.String(), "ADMIN_PUBLIC_ORIGIN is required") || len(f.auth.sessions) != before ||
					response.Header().Get("Access-Control-Allow-Origin") != "" {
					t.Fatal("missing HTTPS config was not rejected clearly before session/CORS effects")
				}
			} else if path == "/api/admin/login" {
				cookies := response.Result().Cookies()
				if len(cookies) != 1 || !cookies[0].Secure {
					t.Fatal("configured HTTPS login did not issue a Secure cookie")
				}
			}
		}
		if !configured {
			r := adminTestRequest(http.MethodGet, "/api/admin/me", "", nil)
			r.Header.Set("Authorization", "Bearer "+adminTestKey)
			r.Header.Del(adminBrowserHeader)
			adminTestResponse(t, f.server.Router(), r, http.StatusOK)
			adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodGet, "/api/admin/me", "", cookie), http.StatusOK)
		}
	}
}

func TestAdminProofRequiredForEveryCookieMethod(t *testing.T) {
	f := newAdminAuthFixture(t, config.Config{})
	cookie := f.login(t)
	server := NewServer(config.Config{}, f.log)
	server.Mount(func(r chi.Router) {
		r.Handle("/api/admin/protected", f.auth.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		})))
	})
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, proof := range [][]string{nil, {""}, {"0"}, {"1, 1"}, {"1", "1"}, {"1"}} {
			r := adminTestRequest(method, "/api/admin/protected", "", cookie)
			r.Header.Del(adminBrowserHeader)
			if proof != nil {
				r.Header[http.CanonicalHeaderKey(adminBrowserHeader)] = proof
			}
			wantStatus := http.StatusForbidden
			if len(proof) == 1 && proof[0] == "1" {
				wantStatus = http.StatusOK
			}
			adminTestResponse(t, server.Router(), r, wantStatus)
			if !f.auth.validSession(cookie.Value) {
				t.Fatal("proof rejection revoked the current session")
			}
		}
	}
	r := adminTestRequest(http.MethodPost, "/api/admin/login", `{"username":"operator","password":"synthetic-password"}`, cookie)
	r.Header.Set("Authorization", "Bearer "+adminTestKey)
	r.Header.Del(adminBrowserHeader)
	adminTestResponse(t, f.server.Router(), r, http.StatusForbidden)
}

func TestAdminLoginRejectsAmbiguousMediaAndOversizedBodies(t *testing.T) {
	f := newAdminAuthFixture(t, config.Config{})
	r := adminTestRequest(http.MethodPost, "/api/admin/login", `{"username":"operator","password":"synthetic-password"}`, nil)
	r.Header.Add("Content-Type", "application/json")
	adminTestResponse(t, f.server.Router(), r, http.StatusUnsupportedMediaType)
	f = newAdminAuthFixture(t, config.Config{RequestBodyLimitBytes: 32})
	r = adminTestRequest(http.MethodPost, "/api/admin/login", `{"username":"operator","password":"synthetic-password"}`, nil)
	adminTestResponse(t, f.server.Router(), r, http.StatusBadRequest)
	if len(f.auth.sessions) != 0 {
		t.Fatal("oversized login body created a session")
	}
}

func TestTrustedProxyIPMiddleware(t *testing.T) {
	for _, test := range []struct {
		name       string
		peer       string
		realIP     []string
		wantIP     string
		wantRemote string
	}{
		{name: "direct ignores all forwarded IPs", peer: "192.0.2.10:54321", realIP: []string{"198.51.100.2"}, wantIP: "192.0.2.10", wantRemote: "192.0.2.10:54321"},
		{name: "nonloopback proxy not trusted", peer: "10.0.0.2:54321", realIP: []string{"198.51.100.2"}, wantIP: "10.0.0.2", wantRemote: "10.0.0.2:54321"},
		{name: "direct IPv6", peer: "[2001:db8::1]:54321", realIP: []string{"198.51.100.2"}, wantIP: "2001:db8::1", wantRemote: "[2001:db8::1]:54321"},
		{name: "loopback IPv4", peer: "127.0.0.1:54321", realIP: []string{"198.51.100.2"}, wantIP: "198.51.100.2", wantRemote: "198.51.100.2"},
		{name: "loopback IPv6", peer: "[::1]:54321", realIP: []string{"2001:0db8::2"}, wantIP: "2001:db8::2", wantRemote: "2001:db8::2"},
		{name: "mapped loopback peer", peer: "[::ffff:127.0.0.1]:54321", realIP: []string{"198.51.100.2"}, wantIP: "198.51.100.2", wantRemote: "198.51.100.2"},
		{name: "loopback missing real IP", peer: "127.0.0.1:54321", wantIP: "127.0.0.1", wantRemote: "127.0.0.1:54321"},
		{name: "empty", peer: "127.0.0.1:54321", realIP: []string{""}, wantIP: "127.0.0.1", wantRemote: "127.0.0.1:54321"},
		{name: "multiple values", peer: "127.0.0.1:54321", realIP: []string{"198.51.100.2", "198.51.100.3"}, wantIP: "127.0.0.1", wantRemote: "127.0.0.1:54321"},
		{name: "list", peer: "127.0.0.1:54321", realIP: []string{"198.51.100.2, 198.51.100.3"}, wantIP: "127.0.0.1", wantRemote: "127.0.0.1:54321"},
		{name: "host and port", peer: "127.0.0.1:54321", realIP: []string{"198.51.100.2:1234"}, wantIP: "127.0.0.1", wantRemote: "127.0.0.1:54321"},
		{name: "bracketed IPv6", peer: "127.0.0.1:54321", realIP: []string{"[::1]"}, wantIP: "127.0.0.1", wantRemote: "127.0.0.1:54321"},
		{name: "hostname", peer: "127.0.0.1:54321", realIP: []string{"client.example"}, wantIP: "127.0.0.1", wantRemote: "127.0.0.1:54321"},
		{name: "padded address", peer: "127.0.0.1:54321", realIP: []string{" 198.51.100.2 "}, wantIP: "127.0.0.1", wantRemote: "127.0.0.1:54321"},
		{name: "invalid socket peer", peer: "not-an-ip", realIP: []string{"198.51.100.2"}, wantIP: "not-an-ip", wantRemote: "not-an-ip"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := trustedProxyIPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if clientIP(r) != test.wantIP || r.RemoteAddr != test.wantRemote {
					t.Fatalf("clientIP=%q RemoteAddr=%q, want %q / %q", clientIP(r), r.RemoteAddr, test.wantIP, test.wantRemote)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			r := httptest.NewRequest(http.MethodPost, "/api/admin/login", nil)
			r.RemoteAddr = test.peer
			r.Header.Set("True-Client-IP", "203.0.113.1")
			r.Header.Set("X-Forwarded-For", "203.0.113.2, 203.0.113.3")
			if test.realIP != nil {
				r.Header[http.CanonicalHeaderKey("X-Real-IP")] = test.realIP
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			if response.Code != http.StatusNoContent {
				t.Fatalf("middleware response = %d", response.Code)
			}
		})
	}
}
