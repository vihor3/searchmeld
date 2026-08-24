package keypool

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/one-search/one-search/backend/internal/model"
	"github.com/one-search/one-search/backend/internal/provider"
)

type fakeStore struct {
	keys           []model.APIKey
	strategy       string
	maxConcurrency int
}

func (s *fakeStore) ListAvailableProviderKeys(ctx context.Context, providerName string) ([]model.APIKey, error) {
	return append([]model.APIKey(nil), s.keys...), nil
}

func (s *fakeStore) ProviderKeySettings(ctx context.Context, providerName string) (string, int, error) {
	return s.strategy, s.maxConcurrency, nil
}

func (s *fakeStore) RecordKeyResult(ctx context.Context, key model.APIKey, success bool, errorType string) error {
	return nil
}

func TestAcquireAllowsConcurrentUseWhenProviderMaxConcurrencyZero(t *testing.T) {
	manager := NewManager(&fakeStore{keys: []model.APIKey{{ID: 1, ProviderName: model.ProviderYou, MaxConcurrency: 0}}})
	releases := make([]func(bool, error), 0, 5)

	for i := 0; i < 5; i++ {
		key, release, err := manager.Acquire(context.Background(), model.ProviderYou)
		if err != nil {
			t.Fatalf("Acquire #%d returned error: %v", i+1, err)
		}
		if key.ID != 1 {
			t.Fatalf("Acquire #%d key ID = %d, want 1", i+1, key.ID)
		}
		releases = append(releases, release)
	}

	for _, release := range releases {
		release(true, nil)
	}
}

func TestAcquireHonorsProviderMaxConcurrency(t *testing.T) {
	manager := NewManager(&fakeStore{
		keys:           []model.APIKey{{ID: 1, ProviderName: model.ProviderYou}, {ID: 2, ProviderName: model.ProviderYou}},
		maxConcurrency: 1,
	})

	_, release, err := manager.Acquire(context.Background(), model.ProviderYou)
	if err != nil {
		t.Fatalf("first Acquire returned error: %v", err)
	}
	defer release(true, nil)

	_, _, err = manager.Acquire(context.Background(), model.ProviderYou)
	if provider.ErrorType(err) != provider.ErrorTypeRateLimited {
		t.Fatalf("second Acquire error type = %q, want %q (err=%v)", provider.ErrorType(err), provider.ErrorTypeRateLimited, err)
	}
}

func TestAcquireHonorsPerKeyMaxConcurrency(t *testing.T) {
	manager := NewManager(&fakeStore{
		keys: []model.APIKey{{ID: 1, ProviderName: model.ProviderYou, MaxConcurrency: 1}},
	})

	_, release, err := manager.Acquire(context.Background(), model.ProviderYou)
	if err != nil {
		t.Fatalf("first Acquire returned error: %v", err)
	}

	_, _, err = manager.Acquire(context.Background(), model.ProviderYou)
	if provider.ErrorType(err) != provider.ErrorTypeRateLimited {
		t.Fatalf("concurrent Acquire error type = %q, want %q (err=%v)", provider.ErrorType(err), provider.ErrorTypeRateLimited, err)
	}

	release(true, nil)
	_, release, err = manager.Acquire(context.Background(), model.ProviderYou)
	if err != nil {
		t.Fatalf("Acquire after release returned error: %v", err)
	}
	release(true, nil)
}

func TestAcquireReservesDailyQuotaAtomically(t *testing.T) {
	manager := NewManager(&fakeStore{
		keys: []model.APIKey{{ID: 1, ProviderName: model.ProviderYou, DailyQuota: 1}},
	})

	const workers = 16
	start := make(chan struct{})
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, release, err := manager.Acquire(context.Background(), model.ProviderYou)
			if err == nil {
				release(true, nil)
			}
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if provider.ErrorType(err) != provider.ErrorTypeRateLimited {
			t.Fatalf("Acquire error type = %q, want %q (err=%v)", provider.ErrorType(err), provider.ErrorTypeRateLimited, err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful Acquires = %d, want 1", successes)
	}
}

func TestAcquireKeepsDailyReservationUntilUsageSnapshotAdvances(t *testing.T) {
	store := &fakeStore{
		keys: []model.APIKey{{ID: 1, ProviderName: model.ProviderYou, DailyQuota: 2}},
	}
	manager := NewManager(store)
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.Local)
	manager.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		_, release, err := manager.Acquire(context.Background(), model.ProviderYou)
		if err != nil {
			t.Fatalf("Acquire #%d returned error: %v", i+1, err)
		}
		release(true, nil)
	}

	_, _, err := manager.Acquire(context.Background(), model.ProviderYou)
	if provider.ErrorType(err) != provider.ErrorTypeRateLimited {
		t.Fatalf("Acquire with lagging usage error type = %q, want %q (err=%v)", provider.ErrorType(err), provider.ErrorTypeRateLimited, err)
	}

	store.keys[0].DailyUsed = 1
	store.keys[0].UsageRequestsTotal = 1
	_, _, err = manager.Acquire(context.Background(), model.ProviderYou)
	if provider.ErrorType(err) != provider.ErrorTypeRateLimited {
		t.Fatalf("Acquire with partially advanced usage error type = %q, want %q (err=%v)", provider.ErrorType(err), provider.ErrorTypeRateLimited, err)
	}
	store.keys[0].DailyUsed = 0
	_, _, err = manager.Acquire(context.Background(), model.ProviderYou)
	if provider.ErrorType(err) != provider.ErrorTypeRateLimited {
		t.Fatalf("Acquire after same-day usage regression error type = %q, want %q (err=%v)", provider.ErrorType(err), provider.ErrorTypeRateLimited, err)
	}

	now = now.AddDate(0, 0, 1)
	store.keys[0].UsageRequestsTotal = 2
	_, release, err := manager.Acquire(context.Background(), model.ProviderYou)
	if err != nil {
		t.Fatalf("Acquire after daily reset returned error: %v", err)
	}
	release(true, nil)
}

func TestAcquireReconcilesMonthlyReservationsAndResetsMonth(t *testing.T) {
	store := &fakeStore{
		keys: []model.APIKey{{ID: 1, ProviderName: model.ProviderYou, MonthlyQuota: 3}},
	}
	manager := NewManager(store)
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.Local)
	manager.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		_, release, err := manager.Acquire(context.Background(), model.ProviderYou)
		if err != nil {
			t.Fatalf("Acquire #%d returned error: %v", i+1, err)
		}
		release(true, nil)
	}

	store.keys[0].MonthlyUsed = 2
	store.keys[0].UsageRequestsTotal = 2
	_, release, err := manager.Acquire(context.Background(), model.ProviderYou)
	if err != nil {
		t.Fatalf("Acquire after monthly snapshot reconciliation returned error: %v", err)
	}
	release(true, nil)

	_, _, err = manager.Acquire(context.Background(), model.ProviderYou)
	if provider.ErrorType(err) != provider.ErrorTypeRateLimited {
		t.Fatalf("Acquire at monthly quota error type = %q, want %q (err=%v)", provider.ErrorType(err), provider.ErrorTypeRateLimited, err)
	}

	now = now.AddDate(0, 1, 0)
	store.keys[0].MonthlyUsed = 0
	store.keys[0].UsageRequestsTotal = 3
	_, release, err = manager.Acquire(context.Background(), model.ProviderYou)
	if err != nil {
		t.Fatalf("Acquire after monthly reset returned error: %v", err)
	}
	release(true, nil)
}

func TestAcquireCarriesUnaccountedReservationAcrossDailyReset(t *testing.T) {
	store := &fakeStore{
		keys: []model.APIKey{{ID: 1, ProviderName: model.ProviderYou, DailyQuota: 1}},
	}
	manager := NewManager(store)
	now := time.Date(2026, time.August, 24, 23, 59, 0, 0, time.Local)
	manager.now = func() time.Time { return now }

	_, release, err := manager.Acquire(context.Background(), model.ProviderYou)
	if err != nil {
		t.Fatalf("Acquire before daily reset returned error: %v", err)
	}
	release(true, nil)

	now = now.Add(2 * time.Minute)
	_, _, err = manager.Acquire(context.Background(), model.ProviderYou)
	if provider.ErrorType(err) != provider.ErrorTypeRateLimited {
		t.Fatalf("Acquire with unaccounted rollover reservation error type = %q, want %q (err=%v)", provider.ErrorType(err), provider.ErrorTypeRateLimited, err)
	}

	store.keys[0].DailyUsed = 1
	store.keys[0].UsageRequestsTotal = 1
	_, _, err = manager.Acquire(context.Background(), model.ProviderYou)
	if provider.ErrorType(err) != provider.ErrorTypeRateLimited {
		t.Fatalf("Acquire after late usage was recorded error type = %q, want %q (err=%v)", provider.ErrorType(err), provider.ErrorTypeRateLimited, err)
	}
}

func TestAcquireExpiresStaleUnaccountedReservation(t *testing.T) {
	store := &fakeStore{
		keys: []model.APIKey{{ID: 1, ProviderName: model.ProviderYou, DailyQuota: 1}},
	}
	manager := NewManager(store)
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.Local)
	manager.now = func() time.Time { return now }

	_, release, err := manager.Acquire(context.Background(), model.ProviderYou)
	if err != nil {
		t.Fatalf("first Acquire returned error: %v", err)
	}
	release(true, nil)

	now = now.Add(maxPendingReservationAge + time.Minute)
	_, release, err = manager.Acquire(context.Background(), model.ProviderYou)
	if err != nil {
		t.Fatalf("Acquire after stale reservation expiry returned error: %v", err)
	}
	release(true, nil)
}

func TestAcquireUsesDatabaseUsagePeriodForDailyReset(t *testing.T) {
	store := &fakeStore{
		keys: []model.APIKey{{
			ID:                 1,
			ProviderName:       model.ProviderYou,
			DailyQuota:         1,
			DailyUsagePeriod:   "2026-08-24",
			MonthlyUsagePeriod: "2026-08",
		}},
	}
	manager := NewManager(store)
	manager.now = func() time.Time {
		return time.Date(2026, time.August, 24, 12, 0, 0, 0, time.Local)
	}

	_, release, err := manager.Acquire(context.Background(), model.ProviderYou)
	if err != nil {
		t.Fatalf("first Acquire returned error: %v", err)
	}
	release(true, nil)

	store.keys[0].DailyUsagePeriod = "2026-08-26"
	store.keys[0].UsageRequestsTotal = 1
	_, release, err = manager.Acquire(context.Background(), model.ProviderYou)
	if err != nil {
		t.Fatalf("Acquire after database daily reset returned error: %v", err)
	}
	release(true, nil)
}
