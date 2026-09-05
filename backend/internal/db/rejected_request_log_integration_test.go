package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/vihor3/searchmeld/backend/internal/model"
	"github.com/vihor3/searchmeld/backend/internal/security"
)

func TestRecordRejectedRequestLogDoesNotAffectUsage(t *testing.T) {
	databaseURL := os.Getenv("SEARCHMELD_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SEARCHMELD_TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := RunMigrations(ctx, pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	suffix := time.Now().UnixNano()
	rawToken := fmt.Sprintf("osr_rejected_log_%d", suffix)
	var tokenID int64
	err = pool.QueryRow(ctx, `
		INSERT INTO api_tokens (name, token_hash, token_prefix, scopes)
		VALUES ($1, $2, $3, ARRAY['search'])
		RETURNING id
	`, "rejected-log-test", security.HashToken(rawToken), "osr_test").Scan(&tokenID)
	if err != nil {
		t.Fatalf("create test token: %v", err)
	}
	requestID := fmt.Sprintf("req-rejected-log-%d", suffix)
	acceptedRequestID := requestID + "-accepted"
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM usage_meter_daily WHERE api_token_id=$1`, tokenID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM usage_daily WHERE api_token_id=$1`, tokenID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM search_requests WHERE request_id IN ($1, $2)`, requestID, acceptedRequestID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM api_tokens WHERE id=$1`, tokenID)
	})

	store := NewStore(pool, security.NewCrypto("rejected-log-integration-test"))
	if _, err := store.FindAPIToken(ctx, rawToken); err != nil {
		t.Fatalf("authenticate test token: %v", err)
	}
	assertDatabaseCount(t, ctx, pool, `SELECT usage_count FROM api_tokens WHERE id=$1`, tokenID, 0)
	assertDatabaseCount(t, ctx, pool, `SELECT COUNT(*) FROM api_tokens WHERE id=$1 AND last_used_at IS NOT NULL`, tokenID, 0)
	call := model.ProviderCallLog{
		ProviderName: model.ProviderTavily,
		Status:       "success",
		ResultCount:  1,
		Usage:        []model.UsageMeasurement{{Unit: "credits", Quantity: 1}},
	}
	err = store.RecordRejectedRequestLog(ctx, model.SearchLogInput{
		RequestID:    requestID,
		APITokenID:   tokenID,
		Operation:    "extract",
		CompatFormat: string(model.CompatFormatTavily),
		CachePolicy:  string(model.CachePolicyBypass),
		Status:       "error",
		ErrorMessage: "api token does not include extract scope",
		RequestJSON:  []byte(`{}`),
		ResponseJSON: []byte(`{}`),
		Calls:        []model.ProviderCallLog{call},
	})
	if err != nil {
		t.Fatalf("record rejected request: %v", err)
	}

	var operation, status, compatFormat, query, mode, errorMessage string
	var requestJSON, responseJSON []byte
	var providers []string
	var storedTokenID int64
	if err := pool.QueryRow(ctx, `
		SELECT operation, status, compat_format, api_token_id, query, mode, providers, error_message, request_json, response_json
		FROM search_requests
		WHERE request_id=$1
	`, requestID).Scan(&operation, &status, &compatFormat, &storedTokenID, &query, &mode, &providers, &errorMessage, &requestJSON, &responseJSON); err != nil {
		t.Fatalf("read rejected request: %v", err)
	}
	if operation != "extract" || status != "error" || compatFormat != "tavily" || storedTokenID != tokenID {
		t.Fatalf("unexpected rejected request row: operation=%s status=%s compat=%s token=%d", operation, status, compatFormat, storedTokenID)
	}
	if query != "" || mode != "" || len(providers) != 0 || string(requestJSON) != "{}" || string(responseJSON) != "{}" || errorMessage != "api token does not include extract scope" {
		t.Fatalf("unexpected rejected metadata: query=%q mode=%q providers=%v request=%s response=%s error=%q", query, mode, providers, requestJSON, responseJSON, errorMessage)
	}

	assertDatabaseCount(t, ctx, pool, `SELECT COUNT(*) FROM search_requests WHERE request_id=$1`, requestID, 1)
	assertDatabaseCount(t, ctx, pool, `SELECT COUNT(*) FROM provider_calls WHERE request_id=$1`, requestID, 0)
	assertDatabaseCount(t, ctx, pool, `SELECT COUNT(*) FROM provider_call_usage WHERE request_id=$1`, requestID, 0)
	assertDatabaseCount(t, ctx, pool, `SELECT COUNT(*) FROM usage_daily WHERE api_token_id=$1`, tokenID, 0)
	assertDatabaseCount(t, ctx, pool, `SELECT COUNT(*) FROM usage_meter_daily WHERE api_token_id=$1`, tokenID, 0)
	assertDatabaseCount(t, ctx, pool, `SELECT usage_count FROM api_tokens WHERE id=$1`, tokenID, 0)
	assertDatabaseCount(t, ctx, pool, `SELECT COUNT(*) FROM api_tokens WHERE id=$1 AND last_used_at IS NOT NULL`, tokenID, 0)

	if err := store.RecordSearchLog(ctx, model.SearchLogInput{
		RequestID:    acceptedRequestID,
		APITokenID:   tokenID,
		Operation:    "extract",
		Query:        "https://example.com",
		Mode:         string(model.SearchModeFallback),
		CompatFormat: string(model.CompatFormatNative),
		Providers:    []string{model.ProviderTavily},
		CachePolicy:  string(model.CachePolicyBypass),
		Status:       "success",
		ResultCount:  1,
		Calls:        []model.ProviderCallLog{call},
	}); err != nil {
		t.Fatalf("record accepted request: %v", err)
	}
	assertDatabaseCount(t, ctx, pool, `SELECT COUNT(*) FROM provider_calls WHERE request_id=$1`, acceptedRequestID, 1)
	assertDatabaseCount(t, ctx, pool, `SELECT COUNT(*) FROM provider_call_usage WHERE request_id=$1`, acceptedRequestID, 1)
	assertDatabaseCount(t, ctx, pool, `SELECT requests_total FROM usage_daily WHERE api_token_id=$1`, tokenID, 1)
	assertDatabaseCount(t, ctx, pool, `SELECT COUNT(*) FROM usage_meter_daily WHERE api_token_id=$1`, tokenID, 1)
	assertDatabaseCount(t, ctx, pool, `SELECT usage_count FROM api_tokens WHERE id=$1`, tokenID, 0)

	if err := store.MarkAPITokenUsed(ctx, tokenID); err != nil {
		t.Fatalf("mark token used: %v", err)
	}
	assertDatabaseCount(t, ctx, pool, `SELECT usage_count FROM api_tokens WHERE id=$1`, tokenID, 1)
	assertDatabaseCount(t, ctx, pool, `SELECT COUNT(*) FROM api_tokens WHERE id=$1 AND last_used_at IS NOT NULL`, tokenID, 1)
	assertDatabaseCount(t, ctx, pool, `SELECT requests_total FROM usage_daily WHERE api_token_id=$1`, tokenID, 1)
}

func assertDatabaseCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, argument interface{}, want int64) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(ctx, query, argument).Scan(&got); err != nil {
		t.Fatalf("query count: %v", err)
	}
	if got != want {
		t.Fatalf("count = %d, want %d; query=%s", got, want, query)
	}
}
