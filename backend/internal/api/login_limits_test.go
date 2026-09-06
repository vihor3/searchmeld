package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vihor3/searchmeld/backend/internal/config"
	"github.com/vihor3/searchmeld/backend/internal/model"
)

// TestMountedLoginUsernameBound counts raw padding and UTF-8 bytes toward the
// username limit. Overlong names must fail before lookup, limiter or audit state,
// while boundary/empty names retain ordinary HTTP failure semantics. A direct
// service call must enforce the same early bound on overlong names.
func TestMountedLoginUsernameBound(t *testing.T) {
	for _, test := range []struct {
		name       string
		username   string
		wantStatus int
	}{
		{name: "at limit", username: strings.Repeat("a", maxLoginUsernameBytes), wantStatus: http.StatusUnauthorized},
		{name: "over limit", username: strings.Repeat("a", maxLoginUsernameBytes+1), wantStatus: http.StatusBadRequest},
		{name: "raw padding counts", username: strings.Repeat(" ", maxLoginUsernameBytes) + "a", wantStatus: http.StatusBadRequest},
		{name: "UTF8 bytes count", username: strings.Repeat("\u00e9", maxLoginUsernameBytes/2+1), wantStatus: http.StatusBadRequest},
		{name: "empty unchanged", wantStatus: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAdminAuthFixture(t, config.Config{RequestBodyLimitBytes: 1024 * 1024})
			body, err := json.Marshal(map[string]string{"username": test.username, "password": "wrong"})
			if err != nil {
				t.Fatalf("encode login: %v", err)
			}
			response := adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodPost, "/api/admin/login", string(body), nil), test.wantStatus)
			if len(f.auth.sessions) != 0 || len(response.Result().Cookies()) != 0 {
				t.Fatal("invalid login issued a session")
			}
			if test.wantStatus == http.StatusBadRequest {
				if len(f.store.userLookups) != 0 || len(f.auth.loginWindows) != 0 || len(f.store.audits) != 0 {
					t.Fatal("oversized username reached account lookup, attempt state or audit storage")
				}
			} else if len(f.store.userLookups) != 1 || len(f.auth.loginWindows) != 1 || len(f.store.audits) != 1 {
				t.Fatal("ordinary credential failure semantics changed")
			}
		})
	}
	// The service also rejects overlong names before touching its store.
	auth := NewAuthService(nil, 0, 0, 0, 0)
	_, _, err := auth.Login(context.Background(), strings.Repeat("a", maxLoginUsernameBytes+1), "wrong", "192.0.2.10")
	if !errors.Is(err, ErrInvalidCredentials) || len(auth.loginWindows) != 0 {
		t.Fatal("service username bound was bypassed")
	}
}

// TestMountedLoginCapacityPreservesActiveLocks checks new names are denied at
// capacity without evicting active locks, including locks beyond their attempt
// window. Existing names still count failures and expired locks permit login.
func TestMountedLoginCapacityPreservesActiveLocks(t *testing.T) {
	f := newAdminAuthFixture(t, config.Config{AdminLoginMaxAttempts: 2})
	if f.auth.loginMaxWindows != maxLoginWindows {
		t.Fatalf("default login capacity = %d, want %d", f.auth.loginMaxWindows, maxLoginWindows)
	}
	f.auth.loginMaxWindows = 3
	// login submits mounted JSON requests through the shared status/header assertions.
	login := func(username, password string, status int) {
		t.Helper()
		body, err := json.Marshal(map[string]string{"username": username, "password": password})
		if err != nil {
			t.Fatalf("encode login: %v", err)
		}
		adminTestResponse(t, f.server.Router(), adminTestRequest(http.MethodPost, "/api/admin/login", string(body), nil), status)
	}
	login(adminTestUsername, "wrong", http.StatusUnauthorized)
	login(adminTestUsername, "wrong", http.StatusTooManyRequests)
	key := loginAttemptKey(adminTestUsername, "192.0.2.10")
	locked := f.auth.loginWindows[key]
	locked.StartedAt = time.Now().Add(-2 * f.auth.loginWindow)
	f.auth.loginWindows[key] = locked
	login("unknown-a", "wrong", http.StatusUnauthorized)
	login("unknown-b", "wrong", http.StatusUnauthorized)
	before := len(f.store.userLookups)
	login("unknown-c", "wrong", http.StatusTooManyRequests)
	login(adminTestUsername, adminTestPassword, http.StatusTooManyRequests)
	if len(f.store.userLookups) != before || len(f.auth.loginWindows) != 3 || f.auth.loginWindows[key] != locked {
		t.Fatal("full capacity admitted a new name or reset an active lock")
	}
	// An in-flight failure must not reset a lock just because its window is old.
	if !f.auth.recordLoginFailure(key) || f.auth.loginWindows[key] != locked {
		t.Fatal("late failure reset an active lockout")
	}
	// Existing admitted names can still accumulate failures at capacity.
	login("unknown-a", "wrong", http.StatusTooManyRequests)
	if len(f.store.userLookups) != before+1 || len(f.auth.sessions) != 0 {
		t.Fatal("capacity rejection changed existing attempt/session semantics")
	}
	locked.LockedUntil = time.Now().Add(-time.Second)
	f.auth.loginWindows[key] = locked
	login(adminTestUsername, adminTestPassword, http.StatusOK)
	if len(f.auth.loginWindows) != 2 || len(f.auth.sessions) != 1 {
		t.Fatal("expired lock did not permit normal password login and bucket cleanup")
	}
}

// TestLoginAttemptExpiryReclaimsUnrelatedNames checks a new login lazily reclaims
// expired attempts and locks while preserving unrelated active state.
func TestLoginAttemptExpiryReclaimsUnrelatedNames(t *testing.T) {
	f := newAdminAuthFixture(t, config.Config{})
	f.auth.loginMaxWindows = 4
	now := time.Now()
	activeLock := loginWindow{StartedAt: now.Add(-time.Hour), Count: 5, LockedUntil: now.Add(time.Hour)}
	f.auth.loginWindows = map[string]loginWindow{
		"expired-idle": {StartedAt: now.Add(-2 * f.auth.loginWindow), Count: 1},
		"expired-lock": {StartedAt: now, Count: 5, LockedUntil: now.Add(-time.Second)},
		"active-idle":  {StartedAt: now, Count: 1},
		"active-lock":  activeLock,
	}
	r := adminTestRequest(http.MethodPost, "/api/admin/login", `{"username":"new-name","password":"wrong"}`, nil)
	adminTestResponse(t, f.server.Router(), r, http.StatusUnauthorized)
	if len(f.auth.loginWindows) != 3 || f.auth.loginWindows["active-lock"] != activeLock {
		t.Fatal("expiry failed to reclaim unrelated names or discarded an active lock")
	}
	for _, key := range []string{"expired-idle", "expired-lock"} {
		if _, exists := f.auth.loginWindows[key]; exists {
			t.Fatalf("expired entry %q retained", key)
		}
	}
	if f.auth.loginWindows["active-idle"].Count != 1 || len(f.store.userLookups) != 1 {
		t.Fatal("expiry changed an active attempt window or blocked reclaimed capacity")
	}
}

// TestLoginWindowExpiryBoundary fixes the pruning instant to assert inclusive
// expiry for idle windows and lock deadlines without discarding a still-active lock.
func TestLoginWindowExpiryBoundary(t *testing.T) {
	auth := NewAuthService(nil, 0, 0, 0, 0)
	now := time.Now()
	auth.loginWindows = map[string]loginWindow{
		"idle-boundary": {StartedAt: now.Add(-auth.loginWindow), Count: 1},
		"lock-boundary": {StartedAt: now, Count: 5, LockedUntil: now},
		"active-lock":   {StartedAt: now.Add(-time.Hour), Count: 5, LockedUntil: now.Add(time.Second)},
	}
	auth.mu.Lock()
	auth.pruneLoginWindows(now)
	auth.mu.Unlock()
	if len(auth.loginWindows) != 1 || auth.loginWindows["active-lock"].Count != 5 {
		t.Fatal("attempt/lock expiry boundary is not inclusive or active lock was evicted")
	}
}

// TestLoginCapacityReservedBeforeFailure checks reservations consume capacity
// before failure recording; late unreserved failures cannot grow the map, while
// failures for an admitted name still count.
func TestLoginCapacityReservedBeforeFailure(t *testing.T) {
	auth := NewAuthService(nil, 0, 0, 0, 0)
	auth.loginMaxWindows = 2
	if auth.loginLocked("first") || auth.loginLocked("second") || !auth.loginLocked("third") {
		t.Fatal("in-flight account lookup reservations did not consume capacity")
	}
	if len(auth.loginWindows) != 2 || !auth.recordLoginFailure("third") || len(auth.loginWindows) != 2 {
		t.Fatal("late new-name failure exceeded capacity")
	}
	if auth.recordLoginFailure("first") || auth.loginWindows["first"].Count != 1 {
		t.Fatal("capacity prevented recording an admitted failure")
	}
}

// TestDisabledLoginLimiterDoesNotRetainWindows checks negative attempt limits skip
// bucket allocation across distinct failures without changing successful login.
func TestDisabledLoginLimiterDoesNotRetainWindows(t *testing.T) {
	f := newAdminAuthFixture(t, config.Config{AdminLoginMaxAttempts: -1})
	f.auth.loginMaxWindows = 1
	for _, username := range []string{"unknown-a", "unknown-b", "unknown-c"} {
		_, _, err := f.auth.Login(context.Background(), username, "wrong", "192.0.2.10")
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("disabled limiter returned %v", err)
		}
	}
	f.login(t)
	if len(f.auth.loginWindows) != 0 || len(f.auth.sessions) != 1 {
		t.Fatal("disabled limiter retained attempt state or changed password login")
	}
}

// TestConcurrentLoginNamesReserveCapacityBeforeLookup holds lookup completions
// behind a channel barrier, asserting the cap includes in-flight names and never
// evicts an active lock. Released failures count once and create no sessions.
func TestConcurrentLoginNamesReserveCapacityBeforeLookup(t *testing.T) {
	const attempts, capacity = 8, 3
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	store := &blockingLoginStore{entered: make(chan string, attempts), release: make(chan struct{})}
	auth := NewAuthService(store, 0, 0, 0, 0)
	auth.loginMaxWindows = capacity
	lockKey := loginAttemptKey("locked", "192.0.2.10")
	locked := loginWindow{StartedAt: time.Now().Add(-time.Hour), Count: 5, LockedUntil: time.Now().Add(time.Hour)}
	auth.loginWindows[lockKey] = locked
	start := make(chan struct{})
	results := make(chan error, attempts)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	for index := 0; index < attempts; index++ {
		workers.Add(1)
		go func(username string) {
			defer workers.Done()
			<-start
			_, _, err := auth.Login(ctx, username, "wrong", "192.0.2.10")
			results <- err
		}(fmt.Sprintf("concurrent-%d", index))
	}
	close(start)
	var admitted []string
	denied := 0
	// Lookups stay blocked until every new name is either reserved or denied.
	for observed := 0; observed < attempts; observed++ {
		select {
		case username := <-store.entered:
			admitted = append(admitted, username)
		case err := <-results:
			if !errors.Is(err, ErrLoginRateLimited) {
				t.Fatalf("unreserved login returned %v", err)
			}
			denied++
		case <-ctx.Done():
			t.Fatal("concurrent login reservations did not complete")
		}
	}
	if len(admitted) != capacity-1 || denied != attempts-(capacity-1) {
		t.Fatalf("admitted=%d denied=%d, want %d reserved lookups", len(admitted), denied, capacity-1)
	}
	auth.mu.Lock()
	reserved := len(auth.loginWindows)
	lockPreserved := auth.loginWindows[lockKey] == locked
	auth.mu.Unlock()
	if reserved != capacity || !lockPreserved {
		t.Fatal("concurrent new names exceeded capacity or evicted an active lock")
	}
	close(store.release)
	for range admitted {
		select {
		case err := <-results:
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("reserved login failure returned %v", err)
			}
		case <-ctx.Done():
			t.Fatal("reserved login lookups did not finish")
		}
	}
	workers.Wait()
	if len(auth.loginWindows) != capacity || auth.loginWindows[lockKey] != locked || len(auth.sessions) != 0 {
		t.Fatal("completed failures changed capacity, the active lock or session state")
	}
	for _, username := range admitted {
		if auth.loginWindows[loginAttemptKey(username, "192.0.2.10")].Count != 1 {
			t.Fatal("an admitted concurrent failure was not counted exactly once")
		}
	}
}

type blockingLoginStore struct {
	AuthStore
	entered chan string
	release chan struct{}
}

// GetAdminByUsername announces entry, then blocks until released or canceled to
// expose reservations before lookup failure. The test buffers entered for every
// attempt because the notification itself is not cancellable.
func (s *blockingLoginStore) GetAdminByUsername(ctx context.Context, username string) (model.AdminUser, error) {
	s.entered <- username
	select {
	case <-s.release:
		return model.AdminUser{}, ErrInvalidCredentials
	case <-ctx.Done():
		return model.AdminUser{}, ctx.Err()
	}
}
