package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vihor3/searchmeld/backend/internal/model"
	"github.com/vihor3/searchmeld/backend/internal/security"
	"golang.org/x/crypto/bcrypt"
)

func TestConnectRequiresSuccessfulPing(t *testing.T) {
	databaseURL := os.Getenv("SEARCHMELD_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SEARCHMELD_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pool, err := Connect(ctx, databaseURL)
	if pool != nil {
		pool.Close()
		t.Fatal("Connect returned a pool without a successful startup ping")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect error = %v, want context.Canceled", err)
	}
}

func TestPGXAdminCredentialsAndNoRows(t *testing.T) {
	ctx, store := newPGXIntegrationStore(t)
	username := `pgx-admin ' $1 \\`
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("pgx-admin-fixture"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash fixture password: %v", err)
	}
	if _, err := store.GetAdminByUsername(ctx, username); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing admin error = %v, want pgx.ErrNoRows", err)
	}
	if created, err := store.EnsureAdmin(ctx, username, string(passwordHash)); err != nil || !created {
		t.Fatalf("create admin: created=%v err=%v", created, err)
	}
	if created, err := store.EnsureAdmin(ctx, username, "must-not-replace-existing-hash"); err != nil || created {
		t.Fatalf("ensure existing admin: created=%v err=%v", created, err)
	}
	user, err := store.GetAdminByUsername(ctx, username)
	if err != nil {
		t.Fatalf("read admin: %v", err)
	}
	if user.ID == 0 || user.Username != username || user.PasswordHash != string(passwordHash) || user.CreatedAt.IsZero() {
		t.Fatal("admin identity, password hash or timestamp changed")
	}
	if exists, err := store.AdminExists(ctx, username); err != nil || !exists {
		t.Fatalf("admin exists: exists=%v err=%v", exists, err)
	}
	if key, err := store.GetAdminAPIKey(ctx); err != nil || key.KeyPrefix != "" || key.CreatedAt != nil || key.UpdatedAt != nil {
		t.Fatalf("missing admin key did not return empty metadata: %v", err)
	}
	key, rawKey, err := store.RotateAdminAPIKey(ctx)
	if err != nil {
		t.Fatalf("create admin key: %v", err)
	}
	if rawKey == "" || key.Key != rawKey || key.CreatedAt == nil || key.UpdatedAt == nil {
		t.Fatal("admin key creation lost its secret or timestamps")
	}
	if err := RunMigrations(ctx, store.pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("replay migrations with persisted credentials: %v", err)
	}
	persistedUser, err := store.GetAdminByUsername(ctx, username)
	if err != nil || !reflect.DeepEqual(persistedUser, user) {
		t.Fatalf("migration replay changed the persisted admin identity or password hash: %v", err)
	}
	foundKey, found, err := store.FindAdminAPIKey(ctx, rawKey)
	if err != nil || !found || foundKey.KeyPrefix != key.KeyPrefix || foundKey.Key != "" {
		t.Fatalf("find admin key: found=%v err=%v", found, err)
	}
	if _, _, err := store.RotateAdminAPIKey(ctx); err != nil {
		t.Fatalf("rotate existing admin key: %v", err)
	}
	if _, found, err := store.FindAdminAPIKey(ctx, rawKey); err != nil || found {
		t.Fatalf("rotated admin key still matched: found=%v err=%v", found, err)
	}
}

func TestPGXTokenArraysAndNullableTimes(t *testing.T) {
	ctx, store := newPGXIntegrationStore(t)
	scopes := []string{"search", "extract", `scope,with"quotes\\`, "NULL"}
	providers := []string{model.ProviderTavily, model.ProviderExa}
	token, rawToken, err := store.CreateAPIToken(ctx, "pgx-token", scopes, providers, 13, 17, 29)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if !reflect.DeepEqual(token.Scopes, scopes) || !reflect.DeepEqual(token.AllowedProviders, providers) || token.LastUsedAt != nil {
		t.Fatal("token arrays or NULL last-used timestamp changed")
	}
	if token.RateLimitPerMin != 13 || token.DailyQuota != 17 || token.MonthlyQuota != 29 || token.CreatedAt.IsZero() || token.UpdatedAt.IsZero() {
		t.Fatal("token limits or timestamps changed")
	}
	revealed, err := store.RevealAPIToken(ctx, token.ID)
	if err != nil || revealed.Token != rawToken || revealed.TokenCiphertext != "" {
		t.Fatalf("token secret round trip failed: %v", err)
	}
	if err := store.MarkAPITokenUsed(ctx, token.ID); err != nil {
		t.Fatalf("mark token used: %v", err)
	}
	found, err := store.FindAPIToken(ctx, rawToken)
	if err != nil || found.ID != token.ID || found.LastUsedAt == nil || found.UsageCount != 1 {
		t.Fatalf("find used token: %v", err)
	}
	if err := store.UpdateAPIToken(ctx, token.ID, "pgx-token-updated", nil, []string{}, 0, 0, 0); err != nil {
		t.Fatalf("update token: %v", err)
	}
	items, err := store.ListAPITokens(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("list tokens: len=%d err=%v", len(items), err)
	}
	item := items[0]
	if item.ID != token.ID || item.Name != "pgx-token-updated" || !reflect.DeepEqual(item.Scopes, []string{"search"}) || item.AllowedProviders == nil || len(item.AllowedProviders) != 0 {
		t.Fatal("default scopes or empty (not NULL) provider array changed")
	}
	if item.Token != "" || item.TokenCiphertext != "" || item.RateLimitPerMin != 0 || item.DailyQuota != 0 || item.MonthlyQuota != 0 {
		t.Fatal("list exposed a secret or zero limits changed")
	}
	if err := store.UpdateAPITokenStatus(ctx, token.ID, "disabled"); err != nil {
		t.Fatalf("disable token: %v", err)
	}
	if _, err := store.FindAPIToken(ctx, rawToken); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("disabled token error = %v, want pgx.ErrNoRows", err)
	}
}

func TestPGXProviderKeyNullableQuota(t *testing.T) {
	ctx, store := newPGXIntegrationStore(t)
	key, err := store.CreateProviderKey(ctx, model.ProviderExa, "pgx-key", "synthetic-provider-secret", "fixture-key-id", "synthetic-service-secret", 3, 11, 19, 31, 2)
	if err != nil {
		t.Fatalf("create provider key: %v", err)
	}
	if key.LastUsedAt != nil || key.CooldownUntil != nil || key.OfficialQuotaCheckedAt != nil || key.OfficialQuotaBalance != nil || key.OfficialQuotaBalanceUSD != nil || key.OfficialQuotaUsedUSD != nil || key.OfficialQuotaTotalQuantity != nil {
		t.Fatal("new provider key lost NULL fields")
	}
	available, err := store.ListAvailableProviderKeys(ctx, model.ProviderExa)
	if err != nil || len(available) != 1 {
		t.Fatalf("list available keys: len=%d err=%v", len(available), err)
	}
	if available[0].Value != "synthetic-provider-secret" || available[0].ExaServiceKey != "synthetic-service-secret" || !available[0].LastUsedAt.IsZero() || !available[0].CooldownUntil.IsZero() {
		t.Fatal("provider key decryption or COALESCE timestamp defaults changed")
	}
	alias, zero := "pgx-key-updated", 0
	updated, err := store.UpdateProviderKey(ctx, key.ID, model.ProviderKeyUpdate{Alias: &alias, DailyQuota: &zero})
	if err != nil || updated.Alias != alias || updated.DailyQuota != 0 || updated.MonthlyQuota != 31 || updated.Weight != 3 || updated.RPMLimit != 11 || updated.MaxConcurrency != 2 {
		t.Fatalf("nullable key patch changed untouched values: %v", err)
	}
	checkedAt := time.Date(2026, 9, 6, 12, 34, 56, 123456000, time.FixedZone("fixture", 8*60*60))
	quota := model.ProviderKeyQuotaResult{
		Provider:      model.ProviderExa,
		Status:        "success",
		Source:        model.QuotaSourceOfficial,
		Confidence:    model.QuotaConfidenceExact,
		Unit:          "credits",
		Balance:       float64Ptr(12.125),
		BalanceUSD:    float64Ptr(0),
		TotalQuantity: float64Ptr(100.5),
		FetchedAt:     checkedAt,
	}
	if err := store.UpdateProviderKeyOfficialQuota(ctx, key.ID, quota); err != nil {
		t.Fatalf("update official quota: %v", err)
	}
	stored, err := store.GetAPIKeyByID(ctx, key.ID)
	if err != nil {
		t.Fatalf("read provider key quota: %v", err)
	}
	if stored.OfficialQuotaBalance == nil || *stored.OfficialQuotaBalance != 12.125 || stored.OfficialQuotaBalanceUSD == nil || *stored.OfficialQuotaBalanceUSD != 0 || stored.OfficialQuotaUsedUSD != nil || stored.OfficialQuotaTotalQuantity == nil || *stored.OfficialQuotaTotalQuantity != 100.5 {
		t.Fatal("quota numeric values lost fractional, zero or NULL distinctions")
	}
	if stored.OfficialQuotaCheckedAt == nil || !stored.OfficialQuotaCheckedAt.Equal(checkedAt) || stored.OfficialQuotaSource != quota.Source || stored.OfficialQuotaConfidence != quota.Confidence {
		t.Fatal("quota timestamp instant or attribution changed")
	}
	if stored.Value != "synthetic-provider-secret" || stored.ExaServiceKey != "synthetic-service-secret" {
		t.Fatal("partial key update changed encrypted secrets")
	}
	if err := store.RecordKeyResult(ctx, stored, true, ""); err != nil {
		t.Fatalf("record successful key use: %v", err)
	}
	if err := store.RecordKeyResult(ctx, stored, false, "rate_limited"); err != nil {
		t.Fatalf("record key cooldown: %v", err)
	}
	updated, err = store.GetProviderKey(ctx, key.ID)
	if err != nil || updated.LastUsedAt == nil || updated.CooldownUntil == nil || updated.Status != "cooling" || updated.TotalSuccesses != 1 || updated.TotalFailures != 1 {
		t.Fatalf("key use counters or nullable timestamps changed: %v", err)
	}
	quota.Balance, quota.BalanceUSD, quota.TotalQuantity = nil, nil, nil
	if err := store.UpdateProviderKeyOfficialQuota(ctx, key.ID, quota); err != nil {
		t.Fatalf("clear nullable quota: %v", err)
	}
	stored, err = store.GetAPIKeyByID(ctx, key.ID)
	if err != nil || stored.OfficialQuotaBalance != nil || stored.OfficialQuotaBalanceUSD != nil || stored.OfficialQuotaTotalQuantity != nil {
		t.Fatalf("cleared quota values did not return NULL: %v", err)
	}
}

func TestPGXSettingsCacheAndAuditJSON(t *testing.T) {
	ctx, store := newPGXIntegrationStore(t)
	settings, err := store.RuntimeSettings(ctx)
	if err != nil {
		t.Fatalf("read runtime settings: %v", err)
	}
	settings.DefaultMode = model.SearchModeFallback
	settings.DefaultProviders = []string{model.ProviderExa, model.ProviderTavily}
	settings.DefaultLimit = 7
	settings.APIAuthRequired = false
	if err := store.UpdateRuntimeSettings(ctx, settings); err != nil {
		t.Fatalf("write runtime settings: %v", err)
	}
	stored, err := store.RuntimeSettings(ctx)
	if err != nil || !reflect.DeepEqual(stored, settings) {
		t.Fatalf("runtime JSONB round trip changed settings: %v", err)
	}
	if payload, found, err := store.GetCache(ctx, "missing"); err != nil || found || payload != nil {
		t.Fatalf("cache miss: found=%v err=%v", found, err)
	}
	for _, payload := range []string{`{"items":["quote'","\\","\u641c\u7d22"],"nested":{"enabled":false,"value":null},"quantity":1.25}`, `null`, `{"updated":true}`} {
		if err := store.SetCache(ctx, "pgx-cache", []byte(payload), 60); err != nil {
			t.Fatalf("set cache: %v", err)
		}
		got, found, err := store.GetCache(ctx, "pgx-cache")
		if err != nil || !found {
			t.Fatalf("get cache: found=%v err=%v", found, err)
		}
		assertPGXJSONEqual(t, got, []byte(payload))
	}
	metadata := map[string]interface{}{"nested": map[string]interface{}{"enabled": false, "value": nil}, "quantity": 1.25, "label": `quote' " \\`}
	if err := store.RecordAuditLog(ctx, model.AuditLogInput{RequestID: "pgx-audit", Action: "fixture", Metadata: metadata}); err != nil {
		t.Fatalf("record audit JSON: %v", err)
	}
	logs, err := store.ListAuditLogs(ctx, 10)
	if err != nil || len(logs) != 1 {
		t.Fatalf("list audit logs: len=%d err=%v", len(logs), err)
	}
	if !reflect.DeepEqual(logs[0].Metadata, metadata) || logs[0].Actor != "admin" || logs[0].CreatedAt.IsZero() {
		t.Fatal("audit JSON or timestamp changed")
	}
	if _, err := store.pool.Exec(ctx, `UPDATE search_cache SET expires_at=$1 WHERE cache_key=$2`, time.Unix(1, 0), "pgx-cache"); err != nil {
		t.Fatalf("expire fixture cache: %v", err)
	}
	if _, found, err := store.GetCache(ctx, "pgx-cache"); err != nil || found {
		t.Fatalf("expired cache still matched: found=%v err=%v", found, err)
	}
}

func TestPGXRequestAccountingAndRollback(t *testing.T) {
	ctx, store := newPGXIntegrationStore(t)
	token, _, err := store.CreateAPIToken(ctx, "pgx-accounting", []string{"search"}, []string{}, 0, 0, 0)
	if err != nil {
		t.Fatalf("create accounting token: %v", err)
	}
	input := model.SearchLogInput{
		RequestID:    "pgx-request",
		APITokenID:   token.ID,
		Query:        `query ' $1 \\`,
		Mode:         string(model.SearchModeSingle),
		CompatFormat: string(model.CompatFormatNative),
		Providers:    []string{model.ProviderExa},
		CachePolicy:  string(model.CachePolicyBypass),
		Status:       "success",
		ResultCount:  3,
		LatencyMS:    125,
		RequestJSON:  []byte(`{"query":"fixture","options":{"value":null}}`),
		ResponseJSON: []byte(`{"results":[{"title":"fixture"}]}`),
		Calls: []model.ProviderCallLog{{
			ProviderName: model.ProviderExa,
			Status:       "success",
			LatencyMS:    100,
			ResultCount:  3,
			Usage: []model.UsageMeasurement{
				{Unit: "credits", Quantity: 1.25, CostUSD: float64Ptr(0.03125), Metadata: map[string]interface{}{"fixture": true}},
				{Unit: "unpriced", Quantity: 2.5},
			},
		}},
	}
	if err := store.RecordSearchLog(ctx, input); err != nil {
		t.Fatalf("record request: %v", err)
	}
	log, calls, err := store.GetSearchLogByRequestID(ctx, input.RequestID)
	if err != nil || len(calls) != 1 {
		t.Fatalf("read request: calls=%d err=%v", len(calls), err)
	}
	if log.Query != input.Query || !reflect.DeepEqual(log.Providers, input.Providers) || log.CreatedAt.IsZero() || log.LatencyMS != 125 || log.Operation != "search" {
		t.Fatal("request values, array or timestamp changed")
	}
	assertPGXJSONEqual(t, log.RequestJSON, input.RequestJSON)
	assertPGXJSONEqual(t, log.ResponseJSON, input.ResponseJSON)
	usage := calls[0].Usage
	if calls[0].ProviderKeyID != 0 || calls[0].AttemptIndex != 1 || len(usage) != 2 {
		t.Fatal("NULL provider key, default attempt or usage count changed")
	}
	if usage[0].Quantity != 1.25 || usage[0].CostUSD == nil || *usage[0].CostUSD != 0.03125 || !reflect.DeepEqual(usage[0].Metadata, input.Calls[0].Usage[0].Metadata) || usage[1].Quantity != 2.5 || usage[1].CostUSD != nil {
		t.Fatal("usage NUMERIC, nullable cost or metadata round trip changed")
	}
	input.RequestID = "pgx-request-second"
	if err := store.RecordSearchLog(ctx, input); err != nil {
		t.Fatalf("upsert second request usage: %v", err)
	}
	summary, err := store.UsageSummary(ctx)
	if err != nil || summary.RequestsTotal != 2 || summary.RequestsSuccess != 2 || summary.ResultsTotal != 6 || summary.AverageLatency != 125 {
		t.Fatalf("aggregate BIGINT/NUMERIC scan changed: summary=%+v err=%v", summary, err)
	}
	billing, err := store.BillingSummarySince(ctx, time.Unix(1, 0))
	if err != nil || len(billing.Units) != 2 {
		t.Fatalf("billing aggregation: units=%d err=%v", len(billing.Units), err)
	}
	if billing.Units[0].Unit != "credits" || billing.Units[0].QuantityTotal != 2.5 || billing.Units[0].CostUSDTotal != 0.0625 || billing.Units[1].Unit != "unpriced" || billing.Units[1].QuantityTotal != 5 || billing.Units[1].CostUSDTotal != 0 {
		t.Fatalf("fractional billing totals changed: %+v", billing.Units)
	}
	input.RequestID = "pgx-request-rollback"
	input.Calls = append(input.Calls, model.ProviderCallLog{ProviderName: model.ProviderExa, Status: "invalid-status"})
	err = store.RecordSearchLog(ctx, input)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("constraint error = %v, want SQLSTATE 23514", err)
	}
	if _, _, err := store.GetSearchLogByRequestID(ctx, input.RequestID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("failed transaction left a request: %v", err)
	}
	assertDatabaseCount(t, ctx, store.pool, `SELECT COUNT(*) FROM provider_calls WHERE request_id=$1`, input.RequestID, 0)
	assertDatabaseCount(t, ctx, store.pool, `SELECT COUNT(*) FROM provider_call_usage WHERE request_id=$1`, input.RequestID, 0)
	assertDatabaseCount(t, ctx, store.pool, `SELECT requests_total FROM usage_daily WHERE api_token_id=$1`, token.ID, 2)
	if err := store.DeleteAPIToken(ctx, token.ID); err != nil {
		t.Fatalf("delete token preserving usage: %v", err)
	}
	assertDatabaseCount(t, ctx, store.pool, `SELECT COUNT(*) FROM api_tokens WHERE id=$1`, token.ID, 0)
	assertDatabaseCount(t, ctx, store.pool, `SELECT COUNT(*) FROM search_requests WHERE api_token_id IS NULL AND request_id=$1`, "pgx-request", 1)
	preserved, err := store.UsageSummary(ctx)
	if err != nil || preserved != summary {
		t.Fatalf("token deletion changed request totals: summary=%+v err=%v", preserved, err)
	}
	var quantity, cost float64
	if err := store.pool.QueryRow(ctx, `SELECT quantity_total::float8, cost_usd_total::float8 FROM usage_meter_daily WHERE api_token_id IS NULL AND unit=$1`, "credits").Scan(&quantity, &cost); err != nil {
		t.Fatalf("read preserved usage meter: %v", err)
	}
	if quantity != 2.5 || cost != 0.0625 {
		t.Fatalf("token deletion changed usage meter: quantity=%v cost=%v", quantity, cost)
	}
}

// Each fixture owns a schema so singleton settings/keys and NULL-token totals
// cannot leak into another test sharing the disposable CI database.
func newPGXIntegrationStore(t *testing.T) (context.Context, *Store) {
	t.Helper()
	databaseURL := os.Getenv("SEARCHMELD_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SEARCHMELD_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	adminPool, err := Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(adminPool.Close)
	schema := fmt.Sprintf("pgx_store_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create fixture schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop fixture schema: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse test database config: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["timezone"] = "UTC"
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("create fixture pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := RunMigrations(ctx, pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("run fixture migrations: %v", err)
	}
	return ctx, NewStore(pool, security.NewCrypto("pgx-integration-fixture"))
}

func assertPGXJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gotValue, wantValue interface{}
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode stored JSON: %v", err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode expected JSON: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatal("JSONB round trip changed the value")
	}
}
