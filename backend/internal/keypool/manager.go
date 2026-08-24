package keypool

import (
	"context"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/one-search/one-search/backend/internal/model"
	"github.com/one-search/one-search/backend/internal/provider"
)

type Store interface {
	ListAvailableProviderKeys(ctx context.Context, providerName string) ([]model.APIKey, error)
	ProviderKeySettings(ctx context.Context, providerName string) (string, int, error)
	RecordKeyResult(ctx context.Context, key model.APIKey, success bool, errorType string) error
}

const maxPendingReservationAge = time.Hour

type Manager struct {
	store          Store
	mu             sync.Mutex
	positions      map[string]int
	states         map[int64]*keyState
	providerStates map[string]*providerState
	now            func() time.Time
}

type keyState struct {
	active              int
	windowStart         time.Time
	windowCount         int
	usageInitialized    bool
	usageObserved       int64
	pendingReservations int64
	pendingLast         time.Time
	dailyPeriod         string
	dailyObserved       int64
	monthlyPeriod       string
	monthlyObserved     int64
}

type providerState struct {
	active int
}

func NewManager(store Store) *Manager {
	rand.Seed(time.Now().UnixNano())
	return &Manager{
		store:          store,
		positions:      map[string]int{},
		states:         map[int64]*keyState{},
		providerStates: map[string]*providerState{},
		now:            time.Now,
	}
}

func (m *Manager) Acquire(ctx context.Context, providerName string) (model.APIKey, func(bool, error), error) {
	keys, err := m.store.ListAvailableProviderKeys(ctx, providerName)
	if err != nil {
		return model.APIKey{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return model.APIKey{}, nil, err
	}
	if len(keys) == 0 {
		return model.APIKey{}, nil, &provider.Error{Type: provider.ErrorTypeNoKey, Message: "no available key for " + providerName}
	}

	strategy, maxConcurrency, err := m.store.ProviderKeySettings(ctx, providerName)
	if err != nil {
		return model.APIKey{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return model.APIKey{}, nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	providerState := m.providerStateFor(providerName)
	if maxConcurrency > 0 && providerState.active >= maxConcurrency {
		return model.APIKey{}, nil, &provider.Error{Type: provider.ErrorTypeRateLimited, Message: "all keys are limited or busy for " + providerName}
	}
	keys = m.orderKeys(providerName, keys, strategy)
	start := m.startIndex(providerName, strategy)
	for attempt := 0; attempt < len(keys); attempt++ {
		index := (start + attempt) % len(keys)
		key := keys[index]
		state := m.stateFor(key.ID, now)
		if !m.canUse(state, key, now) {
			continue
		}
		state.active++
		state.windowCount++
		m.reserveQuota(state, now)
		providerState.active++
		if m.usesPosition(strategy) {
			m.positions[providerName] = (index + 1) % len(keys)
		}
		released := false
		release := func(success bool, err error) {
			m.mu.Lock()
			if !released {
				released = true
				if state.active > 0 {
					state.active--
				}
				if providerState.active > 0 {
					providerState.active--
				}
			}
			m.mu.Unlock()
			errorType := provider.ErrorType(err)
			_ = m.store.RecordKeyResult(ctx, key, success, errorType)
		}
		return key, release, nil
	}
	return model.APIKey{}, nil, &provider.Error{Type: provider.ErrorTypeRateLimited, Message: "all keys are limited or busy for " + providerName}
}

func (m *Manager) orderKeys(providerName string, keys []model.APIKey, strategy string) []model.APIKey {
	ordered := append([]model.APIKey(nil), keys...)
	switch strategy {
	case "least_used":
		sort.SliceStable(ordered, func(i, j int) bool {
			left := ordered[i].TotalSuccesses + ordered[i].TotalFailures
			right := ordered[j].TotalSuccesses + ordered[j].TotalFailures
			if left == right {
				if ordered[i].LastUsedAt.Equal(ordered[j].LastUsedAt) {
					return ordered[i].ID < ordered[j].ID
				}
				return ordered[i].LastUsedAt.Before(ordered[j].LastUsedAt)
			}
			return left < right
		})
	case "random":
		rand.Shuffle(len(ordered), func(i, j int) { ordered[i], ordered[j] = ordered[j], ordered[i] })
	case "weighted_random":
		return weightedKeyOrder(ordered)
	default:
		return ordered
	}
	m.positions[providerName] = 0
	return ordered
}

func (m *Manager) startIndex(providerName, strategy string) int {
	if !m.usesPosition(strategy) {
		return 0
	}
	return m.positions[providerName]
}

func (m *Manager) usesPosition(strategy string) bool {
	switch strategy {
	case "least_used", "random", "weighted_random":
		return false
	default:
		return true
	}
}

func weightedKeyOrder(keys []model.APIKey) []model.APIKey {
	remaining := append([]model.APIKey(nil), keys...)
	ordered := make([]model.APIKey, 0, len(keys))
	for len(remaining) > 0 {
		totalWeight := 0
		for _, key := range remaining {
			weight := key.Weight
			if weight <= 0 {
				weight = 1
			}
			totalWeight += weight
		}
		pick := rand.Intn(totalWeight)
		selected := 0
		for index, key := range remaining {
			weight := key.Weight
			if weight <= 0 {
				weight = 1
			}
			if pick < weight {
				selected = index
				break
			}
			pick -= weight
		}
		ordered = append(ordered, remaining[selected])
		remaining = append(remaining[:selected], remaining[selected+1:]...)
	}
	return ordered
}

func (m *Manager) stateFor(keyID int64, now time.Time) *keyState {
	state := m.states[keyID]
	if state == nil {
		state = &keyState{windowStart: now}
		m.states[keyID] = state
	}
	return state
}

func (m *Manager) providerStateFor(providerName string) *providerState {
	state := m.providerStates[providerName]
	if state == nil {
		state = &providerState{}
		m.providerStates[providerName] = state
	}
	return state
}

func (m *Manager) canUse(state *keyState, key model.APIKey, now time.Time) bool {
	m.syncQuotaState(state, key, now)
	if key.MaxConcurrency > 0 && state.active >= key.MaxConcurrency {
		return false
	}
	if key.RPMLimit > 0 {
		if now.Sub(state.windowStart) >= time.Minute {
			state.windowStart = now
			state.windowCount = 0
		}
		if state.windowCount >= key.RPMLimit {
			return false
		}
	}
	if key.DailyQuota > 0 && state.dailyObserved+state.pendingReservations >= int64(key.DailyQuota) {
		return false
	}
	if key.MonthlyQuota > 0 && state.monthlyObserved+state.pendingReservations >= int64(key.MonthlyQuota) {
		return false
	}
	return true
}

func (m *Manager) syncQuotaState(state *keyState, key model.APIKey, now time.Time) {
	reconcileReservationSnapshot(state, key.UsageRequestsTotal)
	if state.pendingReservations > 0 && state.active == 0 && !state.pendingLast.IsZero() && now.Sub(state.pendingLast) >= maxPendingReservationAge {
		// Normal relay requests are bounded to well under an hour. A reservation
		// still missing after that point means its usage log was lost or failed;
		// retain fail-closed behavior temporarily without poisoning the key forever.
		state.pendingReservations = 0
		state.pendingLast = time.Time{}
		state.usageInitialized = true
		state.usageObserved = nonNegative(key.UsageRequestsTotal)
	}

	dailyPeriod := key.DailyUsagePeriod
	if dailyPeriod == "" {
		dailyPeriod = now.Format("2006-01-02")
	}
	reconcileUsagePeriod(
		&state.dailyPeriod,
		&state.dailyObserved,
		dailyPeriod,
		key.DailyUsed,
	)

	monthlyPeriod := key.MonthlyUsagePeriod
	if monthlyPeriod == "" {
		monthlyPeriod = now.Format("2006-01")
	}
	reconcileUsagePeriod(
		&state.monthlyPeriod,
		&state.monthlyObserved,
		monthlyPeriod,
		key.MonthlyUsed,
	)
}

func (m *Manager) reserveQuota(state *keyState, now time.Time) {
	// A release only means the upstream call finished; its usage log can still be
	// pending. Keep the reservation until a later database snapshot accounts for it.
	state.pendingReservations++
	state.pendingLast = now
}

func reconcileReservationSnapshot(state *keyState, snapshot int64) {
	snapshot = nonNegative(snapshot)
	if !state.usageInitialized {
		state.usageInitialized = true
		state.usageObserved = snapshot
		return
	}
	if snapshot <= state.usageObserved {
		return
	}
	delta := snapshot - state.usageObserved
	state.usageObserved = snapshot
	if delta >= state.pendingReservations {
		state.pendingReservations = 0
		state.pendingLast = time.Time{}
		return
	}
	state.pendingReservations -= delta
}

func reconcileUsagePeriod(period *string, observed *int64, nextPeriod string, currentUsed int64) {
	currentUsed = nonNegative(currentUsed)
	if *period == "" {
		*period = nextPeriod
		*observed = currentUsed
		return
	}
	if nextPeriod == *period {
		if currentUsed > *observed {
			*observed = currentUsed
		}
		return
	}
	if nextPeriod < *period {
		// A query started before a period rollover can complete afterwards. Ignore
		// that older snapshot rather than rolling the quota state backwards.
		return
	}
	*period = nextPeriod
	*observed = currentUsed
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
