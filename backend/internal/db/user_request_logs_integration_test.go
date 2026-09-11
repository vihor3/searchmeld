package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vihor3/searchmeld/backend/internal/model"
)

// TestPGXUserRequestLogMetadataAndIdempotence checks explicit unknown values,
// immutable duplicate entries, migration replay and independent accounting.
func TestPGXUserRequestLogMetadataAndIdempotence(t *testing.T) {
	ctx, store := newPGXIntegrationStore(t)
	items, err := store.ListUserRequestLogs(ctx, 0)
	if err != nil || items == nil || len(items) != 0 {
		t.Fatalf("empty entry list: items=%v err=%v", items, err)
	}
	if _, executionID, err := store.GetUserRequestLog(ctx, 1); !errors.Is(err, pgx.ErrNoRows) || executionID != nil {
		t.Fatalf("missing entry: execution=%v err=%v", executionID, err)
	}
	token, _, err := store.CreateAPIToken(ctx, "observed-name", []string{"search"}, []string{}, 0, 0, 0)
	if err != nil {
		t.Fatalf("create observed token: %v", err)
	}
	before := snapshotUserRequestAccounting(t, ctx, store)
	input := userRequestLogFixture(`entry ' $1 %_ \`)
	input.Operation = "mcp"
	input.AuthType = "api_token"
	input.APITokenID = &token.ID
	input.TokenName = token.Name
	input.HTTPStatus = nil
	input.Completion = "interrupted"
	input.MCPErrorCount = 2
	input.MCPToolErrorCount = 1
	entry := recordUserRequestFixture(t, ctx, store, input)
	assertUserRequestInput(t, entry.UserRequestLogInput, input)
	duplicate := input
	duplicate.TokenName = "must-not-replace-snapshot"
	duplicate.Completion = "completed"
	status := 200
	duplicate.HTTPStatus = &status
	if err := store.RecordUserRequestLog(ctx, duplicate); err != nil {
		t.Fatalf("duplicate entry: %v", err)
	}
	items, err = store.ListUserRequestLogs(ctx, 10)
	if err != nil || len(items) != 1 || items[0].ID != entry.ID {
		t.Fatalf("idempotent entry list: count=%d err=%v", len(items), err)
	}
	assertUserRequestInput(t, items[0].UserRequestLogInput, input)
	for replay := 0; replay < 2; replay++ {
		if err := RunMigrations(ctx, store.pool, filepath.Join("..", "..", "migrations")); err != nil {
			t.Fatalf("replay migrations: %v", err)
		}
	}
	persisted, executionID, err := store.GetUserRequestLog(ctx, entry.ID)
	if err != nil || executionID != nil {
		t.Fatalf("replayed entry detail: execution=%v err=%v", executionID, err)
	}
	assertUserRequestInput(t, persisted.UserRequestLogInput, input)
	assertPGXJSONEqual(t, snapshotUserRequestAccounting(t, ctx, store), before)
}

// TestPGXUserRequestLogExecutionIsolation checks that entry reads/writes and their
// failures cannot replay accounting, alter cache data or change dashboard totals.
func TestPGXUserRequestLogExecutionIsolation(t *testing.T) {
	ctx, store := newPGXIntegrationStore(t)
	execution := userRequestExecutionFixture("execution-independent")
	if err := store.RecordSearchLog(ctx, execution); err != nil {
		t.Fatalf("record execution: %v", err)
	}
	if entries, err := store.ListUserRequestLogs(ctx, 10); err != nil || len(entries) != 0 {
		t.Fatalf("execution write fabricated entry history: count=%d err=%v", len(entries), err)
	}
	if err := store.SetCache(ctx, "entry-accounting-cache", []byte(`{"results":[]}`), 3600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	before := snapshotUserRequestAccounting(t, ctx, store)
	if err := RunMigrations(ctx, store.pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("replay migrations over execution-only history: %v", err)
	}
	if entries, err := store.ListUserRequestLogs(ctx, 10); err != nil || len(entries) != 0 {
		t.Fatalf("migration fabricated entry history: count=%d err=%v", len(entries), err)
	}
	assertPGXJSONEqual(t, snapshotUserRequestAccounting(t, ctx, store), before)
	from := time.Now().UTC().Add(-time.Hour)
	summary, err := store.UsageSummarySince(ctx, from)
	if err != nil || summary.RequestsTotal != 1 {
		t.Fatalf("execution summary: %+v err=%v", summary, err)
	}
	series, err := store.UsageSeriesSince(ctx, from, "day")
	if err != nil {
		t.Fatalf("execution series: %v", err)
	}
	billing, err := store.BillingSummarySince(ctx, from)
	if err != nil {
		t.Fatalf("execution billing: %v", err)
	}
	health, err := store.ProviderHealth(ctx, 15)
	if err != nil {
		t.Fatalf("execution health: %v", err)
	}
	input := userRequestLogFixture(execution.RequestID)
	input.ExecutionRequestID = &execution.RequestID
	entry := recordUserRequestFixture(t, ctx, store, input)
	if err := store.RecordUserRequestLog(ctx, input); err != nil {
		t.Fatalf("repeat entry: %v", err)
	}
	if err := store.RecordSearchLog(ctx, execution); err == nil {
		t.Fatal("duplicate execution unexpectedly succeeded")
	}
	invalidEntry := userRequestLogFixture("entry-write-failure")
	invalidEntry.ExecutionRequestID = &execution.RequestID
	invalidEntry.Operation = "invalid"
	if err := store.RecordUserRequestLog(ctx, invalidEntry); err == nil {
		t.Fatal("invalid entry unexpectedly succeeded")
	}
	failedExecution := userRequestExecutionFixture("execution-write-failure")
	failedExecution.Calls = append(failedExecution.Calls, model.ProviderCallLog{
		ProviderName: model.ProviderExa,
		Status:       "invalid",
	})
	if err := store.RecordSearchLog(ctx, failedExecution); err == nil {
		t.Fatal("invalid provider call unexpectedly committed execution")
	}
	failedInput := userRequestLogFixture("entry-after-execution-failure")
	failedInput.ExecutionRequestID = &failedExecution.RequestID
	failedEntry := recordUserRequestFixture(t, ctx, store, failedInput)
	if _, linked, err := store.GetUserRequestLog(ctx, failedEntry.ID); err != nil || linked != nil {
		t.Fatalf("failed execution link: id=%v err=%v", linked, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.RecordUserRequestLog(canceled, userRequestLogFixture("canceled-entry")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled entry write: %v", err)
	}
	if _, err := store.ListUserRequestLogs(canceled, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled entry list: %v", err)
	}
	if _, _, err := store.GetUserRequestLog(canceled, entry.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled entry detail: %v", err)
	}
	assertDatabaseCount(t, ctx, store.pool, `SELECT COUNT(*) FROM user_request_logs WHERE request_id=$1`, "canceled-entry", 0)
	assertPGXJSONEqual(t, snapshotUserRequestAccounting(t, ctx, store), before)
	afterSummary, err := store.UsageSummarySince(ctx, from)
	if err != nil || afterSummary != summary {
		t.Fatalf("entry changed dashboard summary: %+v err=%v", afterSummary, err)
	}
	afterSeries, err := store.UsageSeriesSince(ctx, from, "day")
	if err != nil || !reflect.DeepEqual(afterSeries, series) {
		t.Fatalf("entry changed dashboard series: %v", err)
	}
	afterBilling, err := store.BillingSummarySince(ctx, from)
	if err != nil || !reflect.DeepEqual(afterBilling, billing) {
		t.Fatalf("entry changed billing: %v", err)
	}
	afterHealth, err := store.ProviderHealth(ctx, 15)
	if err != nil || len(afterHealth) != len(health) {
		t.Fatalf("entry changed provider health: %v", err)
	}
	for index := range health {
		afterHealth[index].LastCheckedAt = health[index].LastCheckedAt
	}
	if !reflect.DeepEqual(afterHealth, health) {
		t.Fatal("entry changed provider health values")
	}
	if err := RunMigrations(ctx, store.pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("replay migrations over execution history: %v", err)
	}
	assertPGXJSONEqual(t, snapshotUserRequestAccounting(t, ctx, store), before)
}

// TestPGXUserRequestLogExactLinks checks missing/intended links, prefix collisions,
// cache/retry/rejected execution details and an execution committed after its entry.
func TestPGXUserRequestLogExactLinks(t *testing.T) {
	ctx, store := newPGXIntegrationStore(t)
	for _, requestID := range []string{"prefix%_:tool", "prefix%_:tool-extra", "prefixXXa:tool", "entry-no-selection", "missing%_:tool-extra"} {
		if err := store.RecordSearchLog(ctx, userRequestExecutionFixture(requestID)); err != nil {
			t.Fatalf("seed prefix execution: %v", err)
		}
	}
	noSelection := recordUserRequestFixture(t, ctx, store, userRequestLogFixture("entry-no-selection"))
	if _, linked, err := store.GetUserRequestLog(ctx, noSelection.ID); err != nil || linked != nil {
		t.Fatalf("entry inferred a link from its own ID: id=%v err=%v", linked, err)
	}
	for _, requestID := range []string{"prefix%_:tool", "missing%_:tool"} {
		input := userRequestLogFixture("entry-" + requestID)
		input.Operation = "mcp"
		input.ExecutionRequestID = &requestID
		entry := recordUserRequestFixture(t, ctx, store, input)
		detail, linked, err := store.GetUserRequestLog(ctx, entry.ID)
		if err != nil || detail.ExecutionRequestID == nil || *detail.ExecutionRequestID != requestID {
			t.Fatalf("exact execution snapshot lost: %v", err)
		}
		if requestID == "missing%_:tool" {
			if linked != nil {
				t.Fatal("missing execution linked to a prefix match")
			}
			continue
		}
		execution, _, err := store.GetSearchLogByRequestID(ctx, requestID)
		if err != nil || linked == nil || *linked != execution.ID {
			t.Fatalf("exact execution link mismatch: %v", err)
		}
	}
	for _, kind := range []string{"cache", "retry", "rejected-extract", "late"} {
		t.Run(kind, func(t *testing.T) {
			execution := userRequestExecutionFixture("linked-" + kind)
			switch kind {
			case "cache":
				execution.CacheHit = true
				execution.Calls = nil
			case "retry":
				execution.Calls[0].AttemptIndex = 2
				execution.Calls = append([]model.ProviderCallLog{{ProviderName: model.ProviderExa, Status: "error", AttemptIndex: 1, WillRetry: true}}, execution.Calls...)
			case "rejected-extract":
				execution.Operation = "extract"
				execution.Query = ""
				execution.Mode = ""
				execution.Providers = nil
				execution.ResultCount = 0
				execution.Status = "error"
				execution.ErrorMessage = "api token does not include extract scope"
				execution.RequestJSON = []byte(`{}`)
				execution.ResponseJSON = []byte(`{}`)
			}
			input := userRequestLogFixture("entry-" + kind)
			input.Operation = execution.Operation
			if kind == "rejected-extract" {
				status := 403
				input.HTTPStatus = &status
				input.Path = "/v1/extract"
			}
			input.ExecutionRequestID = &execution.RequestID
			entry := recordUserRequestFixture(t, ctx, store, input)
			if _, linked, err := store.GetUserRequestLog(ctx, entry.ID); err != nil || linked != nil {
				t.Fatalf("unwritten execution link: id=%v err=%v", linked, err)
			}
			before := snapshotUserRequestAccounting(t, ctx, store)
			if kind == "rejected-extract" {
				if err := store.RecordRejectedRequestLog(ctx, execution); err != nil {
					t.Fatalf("record rejected execution: %v", err)
				}
			} else if err := store.RecordSearchLog(ctx, execution); err != nil {
				t.Fatalf("record linked execution: %v", err)
			}
			_, linked, err := store.GetUserRequestLog(ctx, entry.ID)
			if err != nil || linked == nil {
				t.Fatalf("committed execution link missing: %v", err)
			}
			log, calls, err := store.GetSearchLog(ctx, *linked)
			if err != nil || log.RequestID != execution.RequestID || log.CacheHit != execution.CacheHit {
				t.Fatalf("execution detail changed: %v", err)
			}
			wantCalls := len(execution.Calls)
			if kind == "rejected-extract" {
				wantCalls = 0
				var beforeData, afterData map[string]json.RawMessage
				if err := json.Unmarshal(before, &beforeData); err != nil {
					t.Fatalf("decode prior accounting: %v", err)
				}
				if err := json.Unmarshal(snapshotUserRequestAccounting(t, ctx, store), &afterData); err != nil {
					t.Fatalf("decode rejected accounting: %v", err)
				}
				for _, table := range []string{"provider_calls", "provider_call_usage", "usage_daily", "usage_meter_daily", "api_tokens"} {
					assertPGXJSONEqual(t, afterData[table], beforeData[table])
				}
			}
			if len(calls) != wantCalls || (kind == "retry" && (calls[0].AttemptIndex != 1 || !calls[0].WillRetry || calls[1].AttemptIndex != 2)) {
				t.Fatalf("execution attempts changed: count=%d want=%d", len(calls), wantCalls)
			}
		})
	}
}

// TestPGXUserRequestLogMetadataProjection proves that entry detail needs only an
// execution ID, while an unavailable join remains a real error rather than 404.
func TestPGXUserRequestLogMetadataProjection(t *testing.T) {
	ctx, store := newPGXIntegrationStore(t)
	execution := userRequestExecutionFixture("projection-execution")
	if err := store.RecordSearchLog(ctx, execution); err != nil {
		t.Fatalf("seed projection execution: %v", err)
	}
	input := userRequestLogFixture("projection-entry")
	input.ExecutionRequestID = &execution.RequestID
	entry := recordUserRequestFixture(t, ctx, store, input)
	for _, statement := range []string{
		`ALTER TABLE search_requests DROP COLUMN request_json, DROP COLUMN response_json`,
		`ALTER TABLE provider_calls RENAME TO unavailable_provider_calls`,
	} {
		if _, err := store.pool.Exec(ctx, statement); err != nil {
			t.Fatalf("isolate execution payload dependency: %v", err)
		}
	}
	_, linked, err := store.GetUserRequestLog(ctx, entry.ID)
	if err != nil || linked == nil {
		t.Fatalf("metadata projection accessed execution payloads: %v", err)
	}
	if _, _, err := store.GetSearchLog(ctx, *linked); err == nil || errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("fixture did not make execution payload reads unavailable: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `DROP TABLE search_requests CASCADE`); err != nil {
		t.Fatalf("isolate unavailable execution table: %v", err)
	}
	if _, _, err := store.GetUserRequestLog(ctx, entry.ID); err == nil || errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unavailable join was treated as a missing entry: %v", err)
	}
	items, err := store.ListUserRequestLogs(ctx, 10)
	if err != nil || len(items) != 1 || items[0].ID != entry.ID {
		t.Fatalf("entry list depended on execution reads: count=%d err=%v", len(items), err)
	}
}

// TestPGXUserRequestLogListBounds checks the default/cap and both sort keys using
// tied start times plus an older high-ID row and a newer low-ID row.
func TestPGXUserRequestLogListBounds(t *testing.T) {
	ctx, store := newPGXIntegrationStore(t)
	started := time.Now().UTC().Truncate(time.Microsecond)
	newest := userRequestLogFixture("newest-low-id")
	newest.CreatedAt = started.Add(time.Hour)
	first := recordUserRequestFixture(t, ctx, store, newest)
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO user_request_logs (request_id, created_at, operation, compat_format,
			method, path, auth_type, completion, latency_ms)
		SELECT 'window-' || n::text, $1, 'search', 'native', 'POST', '/v1/search',
			'unknown', 'interrupted', 0
		FROM generate_series(1, 1002) n ORDER BY n
	`, started); err != nil {
		t.Fatalf("seed bounded list: %v", err)
	}
	oldest := userRequestLogFixture("oldest-high-id")
	oldest.CreatedAt = started.Add(-time.Hour)
	recordUserRequestFixture(t, ctx, store, oldest)
	for _, test := range []struct {
		limit int
		want  int
	}{{-1, 100}, {0, 100}, {1, 1}, {23, 23}, {1000, 1000}, {1005, 1000}} {
		t.Run(fmt.Sprintf("limit-%d", test.limit), func(t *testing.T) {
			items, err := store.ListUserRequestLogs(ctx, test.limit)
			if err != nil || len(items) != test.want {
				t.Fatalf("bounded list: count=%d want=%d err=%v", len(items), test.want, err)
			}
			if items[0].ID != first.ID {
				t.Fatal("list ignored request start ordering")
			}
			for index := 1; index < len(items); index++ {
				if items[index].RequestID != fmt.Sprintf("window-%d", 1003-index) || !items[index].CreatedAt.Equal(started) {
					t.Fatal("list ignored descending entry ID for tied start times")
				}
			}
		})
	}
}

// TestPGXUserRequestLogSQLBounds exercises actual PostgreSQL constraints, including
// UTF-8 byte lengths, unmodified exact IDs, int64 latency and both MCP ceilings.
func TestPGXUserRequestLogSQLBounds(t *testing.T) {
	ctx, store := newPGXIntegrationStore(t)
	input := userRequestLogFixture(strings.Repeat("r", model.UserRequestLogIDMaxBytes))
	executionID := strings.Repeat("e", model.UserRequestLogIDMaxBytes)
	input.ExecutionRequestID = &executionID
	input.Method = strings.Repeat("M", model.UserRequestLogMethodMaxBytes)
	input.Path = "/" + strings.Repeat("\u754c", 682) + "a"
	input.TokenName = strings.Repeat("\u754c", 85) + "a"
	input.LatencyMS = 1 << 33
	input.MCPErrorCount = 32
	input.MCPToolErrorCount = 32
	entry := recordUserRequestFixture(t, ctx, store, input)
	assertUserRequestInput(t, entry.UserRequestLogInput, input)
	fallbackID := strings.Repeat("p", 32) + "-" + strings.Repeat("r", 70) + ":" + strings.Repeat("m", 70)
	fallback := userRequestLogFixture("fallback-entry")
	fallback.Operation = "mcp"
	fallback.ExecutionRequestID = &fallbackID
	if err := store.RecordSearchLog(ctx, userRequestExecutionFixture(fallbackID)); err != nil {
		t.Fatalf("seed fallback execution ID: %v", err)
	}
	fallbackEntry := recordUserRequestFixture(t, ctx, store, fallback)
	if _, linked, err := store.GetUserRequestLog(ctx, fallbackEntry.ID); err != nil || linked == nil {
		t.Fatalf("174-byte fallback execution ID was lost: %v", err)
	}
	for _, test := range []struct {
		name   string
		change func(*model.UserRequestLogInput)
	}{
		{"empty request ID", func(in *model.UserRequestLogInput) { in.RequestID = "" }},
		{"long request ID", func(in *model.UserRequestLogInput) {
			in.RequestID = strings.Repeat("r", model.UserRequestLogIDMaxBytes+1)
		}},
		{"empty execution ID", func(in *model.UserRequestLogInput) {
			value := ""
			in.ExecutionRequestID = &value
		}},
		{"long execution ID", func(in *model.UserRequestLogInput) {
			value := strings.Repeat("e", model.UserRequestLogIDMaxBytes+1)
			in.ExecutionRequestID = &value
		}},
		{"multibyte request ID", func(in *model.UserRequestLogInput) { in.RequestID = strings.Repeat("\u754c", 86) }},
		{"long method", func(in *model.UserRequestLogInput) {
			in.Method = strings.Repeat("M", model.UserRequestLogMethodMaxBytes+1)
		}},
		{"empty method", func(in *model.UserRequestLogInput) { in.Method = "" }},
		{"long path", func(in *model.UserRequestLogInput) { in.Path = strings.Repeat("p", model.UserRequestLogPathMaxBytes+1) }},
		{"multibyte path", func(in *model.UserRequestLogInput) { in.Path = strings.Repeat("\u754c", 683) }},
		{"empty path", func(in *model.UserRequestLogInput) { in.Path = "" }},
		{"long IP", func(in *model.UserRequestLogInput) {
			in.ClientIP = strings.Repeat("i", model.UserRequestLogClientIPMaxBytes+1)
		}},
		{"long name", func(in *model.UserRequestLogInput) {
			in.TokenName = strings.Repeat("n", model.UserRequestLogTokenNameMaxBytes+1)
		}},
		{"multibyte name", func(in *model.UserRequestLogInput) { in.TokenName = strings.Repeat("\u754c", 86) }},
		{"unknown operation", func(in *model.UserRequestLogInput) { in.Operation = "other" }},
		{"unknown format", func(in *model.UserRequestLogInput) { in.CompatFormat = "mcp" }},
		{"unknown auth", func(in *model.UserRequestLogInput) { in.AuthType = "bearer" }},
		{"unknown completion", func(in *model.UserRequestLogInput) { in.Completion = "panic" }},
		{"nonpositive Token", func(in *model.UserRequestLogInput) {
			value := int64(0)
			in.APITokenID = &value
		}},
		{"small status", func(in *model.UserRequestLogInput) {
			value := 99
			in.HTTPStatus = &value
		}},
		{"large status", func(in *model.UserRequestLogInput) {
			value := 1000
			in.HTTPStatus = &value
		}},
		{"negative latency", func(in *model.UserRequestLogInput) { in.LatencyMS = -1 }},
		{"negative protocol count", func(in *model.UserRequestLogInput) { in.MCPErrorCount = -1 }},
		{"negative tool count", func(in *model.UserRequestLogInput) { in.MCPToolErrorCount = -1 }},
		{"large protocol count", func(in *model.UserRequestLogInput) { in.MCPErrorCount = 33 }},
		{"large tool count", func(in *model.UserRequestLogInput) { in.MCPToolErrorCount = 33 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := userRequestLogFixture(test.name)
			test.change(&invalid)
			err := store.RecordUserRequestLog(ctx, invalid)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Fatalf("metadata constraint: err=%v, want SQLSTATE 23514", err)
			}
			assertDatabaseCount(t, ctx, store.pool, `SELECT COUNT(*) FROM user_request_logs WHERE request_id=$1`, invalid.RequestID, 0)
		})
	}
}

// TestPGXUserRequestLogTokenSnapshots simulates rename/delete between verified
// authentication and entry persistence, including existing NULL-Token aggregates.
func TestPGXUserRequestLogTokenSnapshots(t *testing.T) {
	ctx, store := newPGXIntegrationStore(t)
	token, rawToken, err := store.CreateAPIToken(ctx, "original-name", []string{"search"}, []string{}, 0, 0, 0)
	if err != nil {
		t.Fatalf("create snapshot token: %v", err)
	}
	if err := store.MarkAPITokenUsed(ctx, token.ID); err != nil {
		t.Fatalf("mark observed token: %v", err)
	}
	for _, tokenID := range []int64{0, token.ID} {
		execution := userRequestExecutionFixture(fmt.Sprintf("snapshot-execution-%d", tokenID))
		execution.APITokenID = tokenID
		if err := store.RecordSearchLog(ctx, execution); err != nil {
			t.Fatalf("seed Token and NULL-Token usage: %v", err)
		}
	}
	input := userRequestLogFixture("snapshot-before-rename")
	input.AuthType = "api_token"
	input.APITokenID = &token.ID
	input.TokenName = token.Name
	original := recordUserRequestFixture(t, ctx, store, input)
	if err := store.UpdateAPIToken(ctx, token.ID, "renamed-token", []string{"search"}, []string{}, 0, 0, 0); err != nil {
		t.Fatalf("rename observed token: %v", err)
	}
	input.RequestID = "snapshot-persisted-after-rename"
	renameRace := recordUserRequestFixture(t, ctx, store, input)
	verified, err := store.FindAPIToken(ctx, rawToken)
	if err != nil || verified.Name != "renamed-token" || verified.UsageCount != 1 || verified.LastUsedAt == nil {
		t.Fatalf("read renamed Token snapshot: %v", err)
	}
	beforeSummary, err := store.UsageSummary(ctx)
	if err != nil || beforeSummary.RequestsTotal != 2 {
		t.Fatalf("snapshot baseline usage: %+v err=%v", beforeSummary, err)
	}
	beforeBilling, err := store.BillingSummarySince(ctx, time.Unix(1, 0))
	if err != nil {
		t.Fatalf("snapshot baseline billing: %v", err)
	}
	if err := store.DeleteAPIToken(ctx, token.ID); err != nil {
		t.Fatalf("delete observed token with aggregate collision: %v", err)
	}
	input.RequestID = "snapshot-persisted-after-delete"
	input.TokenName = verified.Name
	deleteRace := recordUserRequestFixture(t, ctx, store, input)
	for _, test := range []struct {
		entry model.UserRequestLog
		name  string
	}{{original, "original-name"}, {renameRace, "original-name"}, {deleteRace, "renamed-token"}} {
		persisted, _, err := store.GetUserRequestLog(ctx, test.entry.ID)
		if err != nil || persisted.APITokenID == nil || *persisted.APITokenID != token.ID || persisted.TokenName != test.name {
			t.Fatalf("request-time Token snapshot changed: %v", err)
		}
	}
	afterSummary, err := store.UsageSummary(ctx)
	if err != nil || afterSummary != beforeSummary {
		t.Fatalf("Token deletion or entry changed usage totals: %+v err=%v", afterSummary, err)
	}
	afterBilling, err := store.BillingSummarySince(ctx, time.Unix(1, 0))
	if err != nil || !reflect.DeepEqual(afterBilling, beforeBilling) {
		t.Fatalf("Token deletion or entry changed billing: %v", err)
	}
	assertDatabaseCount(t, ctx, store.pool, `SELECT COUNT(*) FROM api_tokens WHERE id=$1`, token.ID, 0)
	assertDatabaseCount(t, ctx, store.pool, `SELECT COUNT(*) FROM search_requests WHERE api_token_id IS NOT NULL AND request_id=$1`, fmt.Sprintf("snapshot-execution-%d", token.ID), 0)
	var mergedRequests int64
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(SUM(requests_total),0) FROM usage_daily WHERE api_token_id IS NULL AND provider_id IS NULL AND provider_key_id IS NULL`).Scan(&mergedRequests); err != nil {
		t.Fatalf("read merged Token usage: %v", err)
	}
	if mergedRequests != 2 {
		t.Fatalf("Token usage merge changed totals: requests=%d", mergedRequests)
	}
	var quantity, cost float64
	if err := store.pool.QueryRow(ctx, `SELECT SUM(quantity_total)::float8, SUM(cost_usd_total)::float8 FROM usage_meter_daily WHERE api_token_id IS NULL AND unit='credits'`).Scan(&quantity, &cost); err != nil {
		t.Fatalf("read merged Token meter: %v", err)
	}
	if quantity != 2.5 || cost != 0.0625 {
		t.Fatalf("Token meter merge changed totals: quantity=%v cost=%v", quantity, cost)
	}
}

// userRequestLogFixture supplies valid metadata with microsecond start precision
// so PostgreSQL timestamp round trips can be compared without rounding noise.
func userRequestLogFixture(requestID string) model.UserRequestLogInput {
	status := 200
	return model.UserRequestLogInput{
		RequestID:    requestID,
		CreatedAt:    time.Now().UTC().Truncate(time.Microsecond),
		Operation:    "search",
		CompatFormat: string(model.CompatFormatNative),
		Method:       "POST",
		Path:         "/v1/search",
		ClientIP:     "2001:db8::1",
		AuthType:     "anonymous",
		HTTPStatus:   &status,
		Completion:   "completed",
		LatencyMS:    25,
	}
}

// userRequestExecutionFixture supplies real fractional usage and payload history
// without invoking a provider or updating authentication's Token-use counter.
func userRequestExecutionFixture(requestID string) model.SearchLogInput {
	return model.SearchLogInput{
		RequestID:    requestID,
		Operation:    "search",
		Query:        "entry accounting fixture",
		Mode:         string(model.SearchModeSingle),
		CompatFormat: string(model.CompatFormatNative),
		Providers:    []string{model.ProviderExa},
		CachePolicy:  string(model.CachePolicyBypass),
		Status:       "success",
		ResultCount:  2,
		LatencyMS:    25,
		RequestJSON:  []byte(`{"query":"fixture"}`),
		ResponseJSON: []byte(`{"results":[{"title":"fixture"}]}`),
		Calls: []model.ProviderCallLog{{
			ProviderName: model.ProviderExa,
			Status:       "success",
			ResultCount:  2,
			LatencyMS:    20,
			Usage:        []model.UsageMeasurement{{Unit: "credits", Quantity: 1.25, CostUSD: float64Ptr(0.03125)}},
		}},
	}
}

// recordUserRequestFixture locates an inserted entry without depending on a list
// limit, returning the same metadata projection exposed by the detail reader.
func recordUserRequestFixture(t *testing.T, ctx context.Context, store *Store, input model.UserRequestLogInput) model.UserRequestLog {
	t.Helper()
	if err := store.RecordUserRequestLog(ctx, input); err != nil {
		t.Fatalf("record entry: %v", err)
	}
	var id int64
	if err := store.pool.QueryRow(ctx, `SELECT id FROM user_request_logs WHERE request_id=$1`, input.RequestID).Scan(&id); err != nil {
		t.Fatalf("locate entry: %v", err)
	}
	entry, _, err := store.GetUserRequestLog(ctx, id)
	if err != nil {
		t.Fatalf("read inserted entry: %v", err)
	}
	return entry
}

// assertUserRequestInput compares every scalar and nullable field without
// printing stored metadata, allowing only an equivalent timestamp location.
func assertUserRequestInput(t *testing.T, got, want model.UserRequestLogInput) {
	t.Helper()
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatal("entry request start time changed")
	}
	got.CreatedAt = want.CreatedAt
	if !reflect.DeepEqual(got, want) {
		t.Fatal("entry metadata changed")
	}
}

// snapshotUserRequestAccounting captures execution, accounting, cache and Token
// counters so a metadata operation cannot silently mutate unrelated persistence.
func snapshotUserRequestAccounting(t *testing.T, ctx context.Context, store *Store) []byte {
	t.Helper()
	var snapshot []byte
	if err := store.pool.QueryRow(ctx, `
		SELECT jsonb_build_object(
			'search_requests', (SELECT jsonb_agg(r ORDER BY id) FROM search_requests r),
			'provider_calls', (SELECT jsonb_agg(r ORDER BY id) FROM provider_calls r),
			'provider_call_usage', (SELECT jsonb_agg(r ORDER BY id) FROM provider_call_usage r),
			'usage_daily', (SELECT jsonb_agg(r ORDER BY id) FROM usage_daily r),
			'usage_meter_daily', (SELECT jsonb_agg(r ORDER BY id) FROM usage_meter_daily r),
			'search_cache', (SELECT jsonb_agg(r ORDER BY cache_key) FROM search_cache r),
			'api_tokens', (SELECT jsonb_agg(jsonb_build_object('id', id, 'usage_count', usage_count, 'last_used_at', last_used_at) ORDER BY id) FROM api_tokens)
		)
	`).Scan(&snapshot); err != nil {
		t.Fatalf("snapshot execution accounting: %v", err)
	}
	return snapshot
}
