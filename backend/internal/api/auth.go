package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vihor3/searchmeld/backend/internal/model"
	"github.com/vihor3/searchmeld/backend/internal/security"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrLoginRateLimited   = errors.New("admin login rate limit exceeded")
)

const extractRejectedKey contextKey = "extract_rejected"

const adminCredentialKey contextKey = "admin_credential"

type adminCredentialKind string

const (
	adminCookieCredential adminCredentialKind = "cookie"
	adminKeyCredential    adminCredentialKind = "key"
)

const (
	maxLoginUsernameBytes = 256
	maxLoginWindows       = 4096
)

type adminCredential struct {
	Kind         adminCredentialKind
	SessionToken string
}

type AuthStore interface {
	GetAdminByUsername(ctx context.Context, username string) (model.AdminUser, error)
	FindAdminAPIKey(ctx context.Context, token string) (model.AdminAPIKey, bool, error)
	FindAPIToken(ctx context.Context, token string) (model.APIToken, error)
	MarkAPITokenUsed(ctx context.Context, id int64) error
	RuntimeSettings(ctx context.Context) (model.RuntimeSettings, error)
}

type AuthService struct {
	store            AuthStore
	sessions         map[string]session
	rateWindows      map[int64]rateWindow
	loginWindows     map[string]loginWindow
	sessionTTL       time.Duration
	loginMaxAttempts int
	loginMaxWindows  int
	loginWindow      time.Duration
	loginLockout     time.Duration
	mu               sync.Mutex
}

type session struct {
	Username  string
	ExpiresAt time.Time
}

type rateWindow struct {
	StartedAt time.Time
	Count     int
}

type loginWindow struct {
	StartedAt   time.Time
	Count       int
	LockedUntil time.Time
}

// NewAuthService creates process-local auth state with fixed-TTL sessions and
// at most maxLoginWindows login buckets when limiting is enabled. Nonpositive
// durations and zero attempts use defaults; negative attempts disable the limiter.
func NewAuthService(store AuthStore, sessionTTL time.Duration, loginMaxAttempts int, loginWindowDuration, loginLockout time.Duration) *AuthService {
	if sessionTTL <= 0 {
		sessionTTL = 24 * time.Hour
	}
	if loginMaxAttempts == 0 {
		loginMaxAttempts = 5
	}
	if loginWindowDuration <= 0 {
		loginWindowDuration = 5 * time.Minute
	}
	if loginLockout <= 0 {
		loginLockout = 15 * time.Minute
	}
	return &AuthService{
		store:            store,
		sessions:         map[string]session{},
		rateWindows:      map[int64]rateWindow{},
		loginWindows:     map[string]loginWindow{},
		sessionTTL:       sessionTTL,
		loginMaxAttempts: loginMaxAttempts,
		loginMaxWindows:  maxLoginWindows,
		loginWindow:      loginWindowDuration,
		loginLockout:     loginLockout,
	}
}

// Login rejects overlong raw usernames before limiter/store access, then creates
// a fixed-TTL in-memory session after password verification. Account lookup and
// password failures map to ErrInvalidCredentials or ErrLoginRateLimited;
// token-generation errors propagate. Success clears the username/IP login bucket.
func (a *AuthService) Login(ctx context.Context, username, password, clientIP string) (string, time.Time, error) {
	if len(username) > maxLoginUsernameBytes {
		return "", time.Time{}, ErrInvalidCredentials
	}
	attemptKey := loginAttemptKey(username, clientIP)
	if a.loginLocked(attemptKey) {
		return "", time.Time{}, ErrLoginRateLimited
	}
	user, err := a.store.GetAdminByUsername(ctx, username)
	if err != nil {
		if a.recordLoginFailure(attemptKey) {
			return "", time.Time{}, ErrLoginRateLimited
		}
		return "", time.Time{}, ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		if a.recordLoginFailure(attemptKey) {
			return "", time.Time{}, ErrLoginRateLimited
		}
		return "", time.Time{}, ErrInvalidCredentials
	}
	token, err := security.RandomToken("adm_")
	if err != nil {
		return "", time.Time{}, err
	}
	expiresAt := time.Now().Add(a.sessionTTL)
	a.mu.Lock()
	delete(a.loginWindows, attemptKey)
	a.sessions[token] = session{Username: username, ExpiresAt: expiresAt}
	a.mu.Unlock()
	return token, expiresAt, nil
}

func (a *AuthService) Logout(token string) bool {
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.sessions[token]; !ok {
		return false
	}
	delete(a.sessions, token)
	return true
}

// requireAdmin validates a supplied header credential as an admin Key without
// Cookie fallback. With no supplied header it requires a live session Cookie and
// browser proof, including reads. It captures the credential for scoped logout
// and reports Key lookup failures as a generic 500 without exposing store errors.
func (a *AuthService) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, supplied := adminHeaderCredential(r); supplied {
			if !strings.HasPrefix(token, "oak_") {
				writeError(w, http.StatusUnauthorized, "admin login required")
				return
			}
			adminKey, ok, err := a.store.FindAdminAPIKey(r.Context(), token)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "could not validate admin api key")
				return
			}
			if !ok {
				writeError(w, http.StatusUnauthorized, "admin login required")
				return
			}
			ctx := context.WithValue(r.Context(), adminActorKey, adminAPIKeyActor(adminKey))
			ctx = context.WithValue(ctx, adminCredentialKey, adminCredential{Kind: adminKeyCredential})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		cookie, err := r.Cookie(adminSessionCookieName)
		if err != nil || !a.validSession(cookie.Value) {
			writeError(w, http.StatusUnauthorized, "admin login required")
			return
		}
		if !requireAdminBrowserProof(w, r) {
			return
		}
		ctx := context.WithValue(r.Context(), adminActorKey, "admin")
		ctx = context.WithValue(ctx, adminCredentialKey, adminCredential{Kind: adminCookieCredential, SessionToken: cookie.Value})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// adminHeaderCredential reports supplied auth headers even when empty or invalid.
// Repeated values return an empty supplied credential; otherwise bearerToken
// selects Bearer before X-API-Key. The presence flag prevents Cookie fallback.
func adminHeaderCredential(r *http.Request) (string, bool) {
	authorization, hasAuthorization := r.Header["Authorization"]
	apiKey, hasAPIKey := r.Header[http.CanonicalHeaderKey("X-API-Key")]
	if !hasAuthorization && !hasAPIKey {
		return "", false
	}
	if len(authorization) > 1 || len(apiKey) > 1 {
		return "", true
	}
	return bearerToken(r), true
}

// requireAPIToken observes only verified identity before rate/scope denial and
// retains deferred admission marks, including the explicit Extract exemption.
func (a *AuthService) requireAPIToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		settings, err := a.store.RuntimeSettings(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !settings.APIAuthRequired {
			observeUserRequestIdentity(r.Context(), userRequestAuthAnonymous, 0, "")
			next.ServeHTTP(w, r)
			return
		}
		token := apiTokenCredential(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "api token required")
			return
		}
		adminKey, ok, err := a.store.FindAdminAPIKey(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if ok {
			observeUserRequestIdentity(r.Context(), userRequestAuthAdminKey, 0, "")
			ctx := context.WithValue(r.Context(), adminActorKey, adminAPIKeyActor(adminKey))
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		apiToken, err := a.store.FindAPIToken(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid api token")
			return
		}
		observeUserRequestIdentity(r.Context(), userRequestAuthToken, apiToken.ID, apiToken.Name)
		extractRejected := false
		defer func() {
			if !extractRejected {
				a.markAPITokenUsed(apiToken.ID)
			}
		}()
		if !a.allowToken(apiToken) {
			writeError(w, http.StatusTooManyRequests, "api token rate limit exceeded")
			return
		}
		ctx := context.WithValue(r.Context(), apiTokenIDKey, apiToken.ID)
		ctx = context.WithValue(ctx, apiTokenKey, apiToken)
		ctx = context.WithValue(ctx, extractRejectedKey, &extractRejected)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *AuthService) markAPITokenUsed(id int64) {
	if id <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = a.store.MarkAPITokenUsed(ctx, id)
}

func (a *AuthService) requireAPITokenScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		scoped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token, ok := APIToken(r.Context()); ok && !apiTokenHasScope(token, scope) {
				writeError(w, http.StatusForbidden, "api token does not include "+scope+" scope")
				return
			}
			next.ServeHTTP(w, r)
		})
		return a.requireAPIToken(scoped)
	}
}

func (a *AuthService) requireTavilyAPITokenScope(scope string) func(http.Handler) http.Handler {
	requireScope := a.requireAPITokenScope(scope)
	return func(next http.Handler) http.Handler {
		return tavilyBodyAPIKeyMiddleware(requireScope(next))
	}
}

func apiTokenHasScope(token model.APIToken, required string) bool {
	required = strings.ToLower(strings.TrimSpace(required))
	for _, rawScope := range token.Scopes {
		scope := strings.ToLower(strings.TrimSpace(rawScope))
		if scope == required || scope == "*" {
			return true
		}
	}
	return false
}

func adminAPIKeyActor(key model.AdminAPIKey) string {
	if key.KeyPrefix == "" {
		return "admin_api_key"
	}
	return "admin_api_key:" + key.KeyPrefix
}

func (a *AuthService) validSession(token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(s.ExpiresAt) {
		delete(a.sessions, token)
		return false
	}
	return true
}

// loginLocked prunes expired state under a.mu, then reports an active lockout or
// lack of room for a new bucket. An admitted new username/IP bucket is reserved
// before account lookup; a disabled limiter returns false without retaining state.
func (a *AuthService) loginLocked(key string) bool {
	if a.loginMaxAttempts < 0 {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.pruneLoginWindows(now)
	if window, ok := a.loginWindows[key]; ok {
		return window.LockedUntil.After(now)
	}
	if len(a.loginWindows) >= a.loginMaxWindows {
		return true
	}
	// Reserve capacity before account lookup so concurrent new names cannot
	// bypass the bound while password verification is in flight.
	a.loginWindows[key] = loginWindow{StartedAt: now}
	return false
}

// Caller holds a.mu. Active lockouts outlive their original attempt window.
func (a *AuthService) pruneLoginWindows(now time.Time) {
	for key, window := range a.loginWindows {
		if !window.LockedUntil.IsZero() {
			if !window.LockedUntil.After(now) {
				delete(a.loginWindows, key)
			}
		} else if !window.StartedAt.Add(a.loginWindow).After(now) {
			delete(a.loginWindows, key)
		}
	}
}

// recordLoginFailure counts failures under a.mu and starts a lockout at the
// configured threshold. Existing locks and new names at capacity return true
// without resetting a lock or adding a bucket; negative limits skip tracking.
func (a *AuthService) recordLoginFailure(key string) bool {
	if a.loginMaxAttempts < 0 {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.pruneLoginWindows(now)
	window, exists := a.loginWindows[key]
	if window.LockedUntil.After(now) || !exists && len(a.loginWindows) >= a.loginMaxWindows {
		return true
	}
	if !exists {
		window = loginWindow{StartedAt: now}
	}
	window.Count++
	if window.Count >= a.loginMaxAttempts {
		window.LockedUntil = now.Add(a.loginLockout)
		a.loginWindows[key] = window
		return true
	}
	a.loginWindows[key] = window
	return false
}

func loginAttemptKey(username, clientIP string) string {
	return strings.ToLower(strings.TrimSpace(username)) + "|" + strings.TrimSpace(clientIP)
}

func (a *AuthService) allowToken(token model.APIToken) bool {
	if token.RateLimitPerMin <= 0 {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	window := a.rateWindows[token.ID]
	if window.StartedAt.IsZero() || now.Sub(window.StartedAt) >= time.Minute {
		window = rateWindow{StartedAt: now, Count: 0}
	}
	if window.Count >= token.RateLimitPerMin {
		a.rateWindows[token.ID] = window
		return false
	}
	window.Count++
	a.rateWindows[token.ID] = window
	return true
}
