package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/vihor3/searchmeld/backend/internal/config"
	"github.com/vihor3/searchmeld/backend/internal/model"
)

type contextKey string

const (
	requestIDKey  contextKey = "request_id"
	apiTokenIDKey contextKey = "api_token_id"
	apiTokenKey   contextKey = "api_token"
	compatAPIKey  contextKey = "compat_api_key"
	adminActorKey contextKey = "admin_actor"
)

const (
	adminSessionCookieName = "searchmeld_admin_session"
	adminBrowserHeader     = "X-SearchMeld-Admin"
)

const adminCookieSecureKey contextKey = "admin_cookie_secure"

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID()
		if clientPrefix := clientRequestIDPrefix(r.Header.Get("X-Request-ID")); clientPrefix != "" {
			// search_requests.request_id is unique and also anchors usage/billing
			// writes. Preserve a bounded client correlation prefix, but always add a
			// server-generated suffix so a reused header cannot roll back accounting.
			requestID = clientPrefix + "-" + requestID
		}
		ctx := context.WithValue(r.Context(), requestIDKey, requestID)
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func clientRequestIDPrefix(value string) string {
	value = strings.TrimSpace(value)
	var prefix strings.Builder
	for index := 0; index < len(value) && prefix.Len() < 32; index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			prefix.WriteByte(character)
		}
	}
	return strings.Trim(prefix.String(), "-_.")
}

// corsMiddleware applies the public CORS policy, including its wildcard and
// OPTIONS behavior. Admin paths bypass it so adminBrowserMiddleware owns their
// separate origin and preflight checks.
func corsMiddleware(origins []string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	allowAll := false
	for _, origin := range origins {
		if origin == "*" {
			allowAll = true
		}
		allowed[origin] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isAdminPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			origin := r.Header.Get("Origin")
			if allowAll || allowed[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				if allowAll && origin == "" {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				}
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, X-Request-ID, Mcp-Session-Id, Mcp-Protocol-Version, Last-Event-ID")
				w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID, Mcp-Session-Id")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isAdminPath matches the admin namespace at a path-segment boundary, excluding
// similarly prefixed public paths.
func isAdminPath(path string) bool {
	return path == "/api/admin" || strings.HasPrefix(path, "/api/admin/")
}

// adminBrowserMiddleware applies admin-only no-store and preflight policy,
// allowing supplied Origins only when they match the normalized canonical origin
// or a valid exact CORS entry. The Cookie Secure decision uses configured origin
// or direct TLS, never forwarded scheme/host headers. HTTPS Origins over plain HTTP
// require publicOrigin even when allowlisted; auth and browser proof remain downstream.
func adminBrowserMiddleware(publicOrigin string, corsOrigins []string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, raw := range corsOrigins {
		if origin, err := config.NormalizeHTTPOrigin(raw); err == nil {
			allowed[origin] = true
		}
	}
	configuredOrigin := ""
	var configErr error
	if publicOrigin != "" {
		configuredOrigin, configErr = config.NormalizeHTTPOrigin(publicOrigin)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isAdminPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Add("Vary", "Origin")
			if configErr != nil {
				writeError(w, http.StatusInternalServerError, "invalid admin public origin configuration")
				return
			}
			targetOrigin := configuredOrigin
			if targetOrigin == "" {
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				var err error
				targetOrigin, err = config.NormalizeHTTPOrigin(scheme + "://" + r.Host)
				if err != nil {
					writeError(w, http.StatusForbidden, "invalid admin request origin")
					return
				}
			}
			origins, supplied := r.Header["Origin"]
			if supplied {
				if len(origins) != 1 || strings.HasSuffix(origins[0], "/") {
					writeError(w, http.StatusForbidden, "admin origin not allowed")
					return
				}
				origin, err := config.NormalizeHTTPOrigin(origins[0])
				if err != nil || (origin != targetOrigin && !allowed[origin]) {
					writeError(w, http.StatusForbidden, "admin origin not allowed")
					return
				}
				if configuredOrigin == "" && r.TLS == nil && strings.HasPrefix(origin, "https://") {
					writeError(w, http.StatusForbidden, "ADMIN_PUBLIC_ORIGIN is required for HTTPS admin access behind a proxy")
					return
				}
				w.Header().Set("Access-Control-Allow-Origin", origins[0])
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID")
			}
			if r.Method == http.MethodOptions {
				w.Header().Add("Vary", "Access-Control-Request-Method")
				w.Header().Add("Vary", "Access-Control-Request-Headers")
				if !supplied || !validAdminPreflight(r) {
					writeError(w, http.StatusForbidden, "admin preflight not allowed")
					return
				}
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, X-Request-ID, "+adminBrowserHeader)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			ctx := context.WithValue(r.Context(), adminCookieSecureKey, r.TLS != nil || strings.HasPrefix(targetOrigin, "https://"))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// validAdminPreflight accepts exactly one supported request method and only
// permitted requested header names. An absent header list is allowed; Origin
// trust and actual request credentials are checked separately.
func validAdminPreflight(r *http.Request) bool {
	methods := r.Header.Values("Access-Control-Request-Method")
	if len(methods) != 1 {
		return false
	}
	switch methods[0] {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
	default:
		return false
	}
	for _, value := range r.Header.Values("Access-Control-Request-Headers") {
		for _, name := range strings.Split(value, ",") {
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "authorization", "content-type", "x-api-key", "x-request-id", "x-searchmeld-admin":
			default:
				return false
			}
		}
	}
	return true
}

// requireAdminBrowserProof requires exactly one X-SearchMeld-Admin value of "1",
// otherwise writing 403 and returning false. This is a browser CSRF control,
// not an authentication credential.
func requireAdminBrowserProof(w http.ResponseWriter, r *http.Request) bool {
	values := r.Header.Values(adminBrowserHeader)
	if len(values) != 1 || values[0] != "1" {
		writeError(w, http.StatusForbidden, "admin browser proof required")
		return false
	}
	return true
}

// adminCookieSecure reads the transport decision from admin middleware, falling
// back only to direct TLS when no decision is in context.
func adminCookieSecure(r *http.Request) bool {
	if secure, ok := r.Context().Value(adminCookieSecureKey).(bool); ok {
		return secure
	}
	return r.TLS != nil
}

// trustedProxyIPMiddleware accepts one valid X-Real-IP from a loopback socket
// peer, ignoring all other forwarding headers. The bundled loopback proxy must
// sanitize that header; other peers retain their socket address.
func trustedProxyIPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer := net.ParseIP(clientIP(r))
		if peer != nil && peer.IsLoopback() {
			values := r.Header.Values("X-Real-IP")
			if len(values) == 1 {
				if ip := net.ParseIP(values[0]); ip != nil {
					r.RemoteAddr = ip.String()
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'self'; object-src 'none'; frame-ancestors 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")
		next.ServeHTTP(w, r)
	})
}

func bodyLimitMiddleware(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limit > 0 && r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func loggingMiddleware(log requestLogger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(recorder, r)
			log.Info("http_request", map[string]interface{}{
				"method":     r.Method,
				"path":       r.URL.Path,
				"status":     recorder.status,
				"latency_ms": time.Since(start).Milliseconds(),
				"request_id": RequestID(r.Context()),
			})
		})
	}
}

type requestLogger interface {
	Info(message string, fields map[string]interface{})
	Error(message string, fields map[string]interface{})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

// Unwrap lets route-local observers and ResponseController reach the original
// transport without adding unsupported optional interfaces to this wrapper.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey).(string)
	return value
}

func APITokenID(ctx context.Context) int64 {
	value, _ := ctx.Value(apiTokenIDKey).(int64)
	return value
}

func APIToken(ctx context.Context) (model.APIToken, bool) {
	value, ok := ctx.Value(apiTokenKey).(model.APIToken)
	return value, ok
}

func AdminActor(ctx context.Context) string {
	value, _ := ctx.Value(adminActorKey).(string)
	if value == "" {
		return "admin"
	}
	return value
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return key
	}
	return ""
}

// apiTokenCredential returns the normal header credential first, then a
// compatibility credential injected by a narrowly scoped route middleware.
// Admin authentication intentionally continues to use bearerToken directly.
func apiTokenCredential(r *http.Request) string {
	if token := bearerToken(r); token != "" {
		return token
	}
	token, _ := r.Context().Value(compatAPIKey).(string)
	return strings.TrimSpace(token)
}

// tavilyBodyAPIKeyMiddleware accepts the legacy Tavily JSON authentication
// shape on Tavily-compatible routes only. The body is restored so the handler
// can decode it normally, and header credentials retain precedence.
func tavilyBodyAPIKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearerToken(r) != "" || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}

		body, err := readBody(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid body")
			return
		}
		var payload struct {
			APIKey string `json:"api_key"`
		}
		if err := json.Unmarshal(body, &payload); err == nil {
			if token := strings.TrimSpace(payload.APIKey); token != "" {
				ctx := context.WithValue(r.Context(), compatAPIKey, token)
				r = r.WithContext(ctx)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func newRequestID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(buf)
}
