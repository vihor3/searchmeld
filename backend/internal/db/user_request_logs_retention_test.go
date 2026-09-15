package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vihor3/searchmeld/backend/internal/model"
)

// TestPGXUserRequestLogRetention checks independent entry/execution expiration,
// existing cascades, audit counts and unchanged durable usage/cache data.
func TestPGXUserRequestLogRetention(t *testing.T) {
	ctx, store := newPGXIntegrationStore(t)
	recent := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	old := recent.Add(-6 * 24 * time.Hour)
	for _, fixture := range []struct {
		requestID string
		createdAt time.Time
	}{
		{"old-linked", old},
		{"new-entry-old-execution", old},
		{"old-execution-only", old},
		{"new-linked", recent},
		{"old-entry-new-execution", recent},
		{"new-execution-only", recent},
	} {
		if err := store.RecordSearchLog(ctx, userRequestExecutionFixture(fixture.requestID)); err != nil {
			t.Fatalf("seed retained execution: %v", err)
		}
		if _, err := store.pool.Exec(ctx, `UPDATE search_requests SET created_at=$1 WHERE request_id=$2`, fixture.createdAt, fixture.requestID); err != nil {
			t.Fatalf("date retained execution: %v", err)
		}
	}
	entries := map[string]model.UserRequestLog{}
	for _, fixture := range []struct {
		requestID string
		createdAt time.Time
		execution string
	}{
		{"old-linked", old, "old-linked"},
		{"old-entry-only", old, ""},
		{"old-entry-new-execution", old, "old-entry-new-execution"},
		{"new-linked", recent, "new-linked"},
		{"new-entry-only", recent, ""},
		{"new-entry-old-execution", recent, "new-entry-old-execution"},
	} {
		input := userRequestLogFixture(fixture.requestID)
		input.CreatedAt = fixture.createdAt
		if fixture.execution != "" {
			input.ExecutionRequestID = &fixture.execution
		}
		entries[fixture.requestID] = recordUserRequestFixture(t, ctx, store, input)
	}
	for _, fixture := range []struct {
		requestID string
		createdAt time.Time
	}{{"old-audit", old}, {"new-audit", recent}} {
		if _, err := store.pool.Exec(ctx, `INSERT INTO audit_logs (request_id, action, created_at) VALUES ($1, 'retention-fixture', $2)`, fixture.requestID, fixture.createdAt); err != nil {
			t.Fatalf("seed retained audit: %v", err)
		}
	}
	if err := store.SetCache(ctx, "retention-cache", []byte(`{"results":[]}`), 3600); err != nil {
		t.Fatalf("seed retained cache: %v", err)
	}
	beforeDurable := snapshotUserRequestDurableUsage(t, ctx, store)
	beforeSummary, err := store.UsageSummary(ctx)
	if err != nil || beforeSummary.RequestsTotal != 6 {
		t.Fatalf("retention baseline usage: %+v err=%v", beforeSummary, err)
	}
	searchDeleted, auditDeleted, userRequestDeleted, err := store.DeleteOldLogs(ctx, 3)
	if err != nil || searchDeleted != 3 || auditDeleted != 1 || userRequestDeleted != 3 {
		t.Fatalf("retention counts: search=%d audit=%d entry=%d err=%v", searchDeleted, auditDeleted, userRequestDeleted, err)
	}
	for _, requestID := range []string{"old-linked", "old-entry-only", "old-entry-new-execution"} {
		if _, _, err := store.GetUserRequestLog(ctx, entries[requestID].ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("old entry survived retention: %v", err)
		}
	}
	for _, requestID := range []string{"old-linked", "new-entry-old-execution", "old-execution-only"} {
		if _, _, err := store.GetSearchLogByRequestID(ctx, requestID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("old execution survived retention: %v", err)
		}
		assertDatabaseCount(t, ctx, store.pool, `SELECT COUNT(*) FROM provider_calls WHERE request_id=$1`, requestID, 0)
		assertDatabaseCount(t, ctx, store.pool, `SELECT COUNT(*) FROM provider_call_usage WHERE request_id=$1`, requestID, 0)
	}
	for _, requestID := range []string{"new-linked", "old-entry-new-execution", "new-execution-only"} {
		if _, calls, err := store.GetSearchLogByRequestID(ctx, requestID); err != nil || len(calls) != 1 || len(calls[0].Usage) != 1 {
			t.Fatalf("recent execution/calls were removed with an entry: %v", err)
		}
	}
	persisted, linked, err := store.GetUserRequestLog(ctx, entries["new-entry-old-execution"].ID)
	if err != nil || linked != nil || persisted.ExecutionRequestID == nil || *persisted.ExecutionRequestID != "new-entry-old-execution" {
		t.Fatalf("retained entry lost unavailable execution identity: id=%v err=%v", linked, err)
	}
	if _, linked, err := store.GetUserRequestLog(ctx, entries["new-linked"].ID); err != nil || linked == nil {
		t.Fatalf("retained execution link missing: %v", err)
	}
	if _, linked, err := store.GetUserRequestLog(ctx, entries["new-entry-only"].ID); err != nil || linked != nil {
		t.Fatalf("retained standalone entry changed: id=%v err=%v", linked, err)
	}
	assertDatabaseCount(t, ctx, store.pool, `SELECT COUNT(*) FROM audit_logs WHERE request_id=$1`, "new-audit", 1)
	assertDatabaseCount(t, ctx, store.pool, `SELECT COUNT(*) FROM audit_logs WHERE request_id=$1`, "old-audit", 0)
	afterSummary, err := store.UsageSummary(ctx)
	if err != nil || afterSummary != beforeSummary {
		t.Fatalf("retention changed durable usage: %+v err=%v", afterSummary, err)
	}
	assertPGXJSONEqual(t, snapshotUserRequestDurableUsage(t, ctx, store), beforeDurable)
	retainedSummary, err := store.UsageSummarySince(ctx, old.Add(-time.Hour))
	if err != nil || retainedSummary.RequestsTotal != 3 {
		t.Fatalf("retained execution summary changed: %+v err=%v", retainedSummary, err)
	}
	retainedBilling, err := store.BillingSummarySince(ctx, old.Add(-time.Hour))
	if err != nil || len(retainedBilling.Units) != 1 || retainedBilling.Units[0].QuantityTotal != 3.75 {
		t.Fatalf("retention did not preserve existing detailed billing cascade: %v", err)
	}
	searchDeleted, auditDeleted, userRequestDeleted, err = store.DeleteOldLogs(ctx, 3)
	if err != nil || searchDeleted != 0 || auditDeleted != 0 || userRequestDeleted != 0 {
		t.Fatalf("repeat retention counts: search=%d audit=%d entry=%d err=%v", searchDeleted, auditDeleted, userRequestDeleted, err)
	}
}

// TestPGXUserRequestLogRetentionFallback checks the three-day fallback and a
// cleanup pass where only entry records expire, without sleeps near the cutoff.
func TestPGXUserRequestLogRetentionFallback(t *testing.T) {
	for _, retentionDays := range []int{0, -2} {
		ctx, store := newPGXIntegrationStore(t)
		recent := userRequestLogFixture("fallback-recent")
		recent.CreatedAt = recent.CreatedAt.Add(-2 * 24 * time.Hour)
		retained := recordUserRequestFixture(t, ctx, store, recent)
		old := userRequestLogFixture("fallback-old")
		old.CreatedAt = old.CreatedAt.Add(-4 * 24 * time.Hour)
		expired := recordUserRequestFixture(t, ctx, store, old)
		searchDeleted, auditDeleted, userRequestDeleted, err := store.DeleteOldLogs(ctx, retentionDays)
		if err != nil || searchDeleted != 0 || auditDeleted != 0 || userRequestDeleted != 1 {
			t.Fatalf("entry-only fallback %d: search=%d audit=%d entry=%d err=%v", retentionDays, searchDeleted, auditDeleted, userRequestDeleted, err)
		}
		if _, _, err := store.GetUserRequestLog(ctx, retained.ID); err != nil {
			t.Fatalf("fallback removed two-day entry: %v", err)
		}
		if _, _, err := store.GetUserRequestLog(ctx, expired.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("fallback retained four-day entry: %v", err)
		}
	}
}

// TestPGXUserRequestLogRetentionErrors forces each independent delete to fail in
// a disposable schema, preserving completed counts and leaving later rows intact.
func TestPGXUserRequestLogRetentionErrors(t *testing.T) {
	for _, stage := range []string{"search", "audit", "entry"} {
		t.Run(stage, func(t *testing.T) {
			ctx, store := newPGXIntegrationStore(t)
			old := time.Now().UTC().Add(-7 * 24 * time.Hour)
			if err := store.RecordSearchLog(ctx, userRequestExecutionFixture("cleanup-error")); err != nil {
				t.Fatalf("seed cleanup execution: %v", err)
			}
			if _, err := store.pool.Exec(ctx, `UPDATE search_requests SET created_at=$1`, old); err != nil {
				t.Fatalf("date cleanup execution: %v", err)
			}
			if _, err := store.pool.Exec(ctx, `INSERT INTO audit_logs (action, created_at) VALUES ('cleanup-error', $1)`, old); err != nil {
				t.Fatalf("seed cleanup audit: %v", err)
			}
			input := userRequestLogFixture("cleanup-error")
			input.CreatedAt = old
			recordUserRequestFixture(t, ctx, store, input)
			var statement string
			var wantSearch, wantAudit int64
			switch stage {
			case "search":
				statement = `ALTER TABLE search_requests RENAME TO retained_search_requests`
			case "audit":
				statement = `ALTER TABLE audit_logs RENAME TO retained_audit_logs`
				wantSearch = 1
			case "entry":
				statement = `ALTER TABLE user_request_logs RENAME TO retained_user_request_logs`
				wantSearch, wantAudit = 1, 1
			}
			if _, err := store.pool.Exec(ctx, statement); err != nil {
				t.Fatalf("isolate failing cleanup table: %v", err)
			}
			searchDeleted, auditDeleted, userRequestDeleted, err := store.DeleteOldLogs(ctx, 3)
			if err == nil || searchDeleted != wantSearch || auditDeleted != wantAudit || userRequestDeleted != 0 {
				t.Fatalf("partial cleanup counts: search=%d audit=%d entry=%d err=%v", searchDeleted, auditDeleted, userRequestDeleted, err)
			}
			query := `SELECT COUNT(*) FROM user_request_logs WHERE request_id=$1`
			if stage == "entry" {
				query = `SELECT COUNT(*) FROM retained_user_request_logs WHERE request_id=$1`
			}
			assertDatabaseCount(t, ctx, store.pool, query, input.RequestID, 1)
		})
	}
}

// snapshotUserRequestDurableUsage compares complete daily aggregates and cache
// rows, whose lifetimes remain independent of execution and entry retention.
func snapshotUserRequestDurableUsage(t *testing.T, ctx context.Context, store *Store) []byte {
	t.Helper()
	var snapshot []byte
	if err := store.pool.QueryRow(ctx, `
		SELECT jsonb_build_object(
			'usage_daily', (SELECT jsonb_agg(r ORDER BY id) FROM usage_daily r),
			'usage_meter_daily', (SELECT jsonb_agg(r ORDER BY id) FROM usage_meter_daily r),
			'search_cache', (SELECT jsonb_agg(r ORDER BY cache_key) FROM search_cache r)
		)
	`).Scan(&snapshot); err != nil {
		t.Fatalf("snapshot durable usage: %v", err)
	}
	return snapshot
}
