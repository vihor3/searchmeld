package db

import (
	"context"

	"github.com/vihor3/searchmeld/backend/internal/model"
)

// RecordUserRequestLog requires sanitized request metadata and lets schema
// constraints reject out-of-bounds values. It writes no execution/accounting data;
// repeated request IDs preserve the first row without clipping correlation IDs.
func (s *Store) RecordUserRequestLog(ctx context.Context, input model.UserRequestLogInput) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_request_logs (
			request_id, created_at, operation, compat_format, method, path, client_ip,
			auth_type, api_token_id, token_name, http_status, completion, latency_ms,
			mcp_error_count, mcp_tool_error_count, execution_request_id
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (request_id) DO NOTHING
	`, input.RequestID, input.CreatedAt, input.Operation, input.CompatFormat, input.Method,
		input.Path, input.ClientIP, input.AuthType, input.APITokenID, input.TokenName,
		input.HTTPStatus, input.Completion, input.LatencyMS, input.MCPErrorCount,
		input.MCPToolErrorCount, input.ExecutionRequestID)
	return err
}

// ListUserRequestLogs returns a non-nil metadata slice in newest-first order.
// Its default of 100 and cap of 1000 match the execution-log storage boundary.
func (s *Store) ListUserRequestLogs(ctx context.Context, limit int) ([]model.UserRequestLog, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, request_id, created_at, operation, compat_format, method, path,
		       client_ip, auth_type, api_token_id, token_name, http_status, completion,
		       latency_ms, mcp_error_count, mcp_tool_error_count, execution_request_id
		FROM user_request_logs ORDER BY created_at DESC, id DESC LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []model.UserRequestLog{}
	for rows.Next() {
		var item model.UserRequestLog
		if err := rows.Scan(&item.ID, &item.RequestID, &item.CreatedAt, &item.Operation,
			&item.CompatFormat, &item.Method, &item.Path, &item.ClientIP, &item.AuthType,
			&item.APITokenID, &item.TokenName, &item.HTTPStatus, &item.Completion,
			&item.LatencyMS, &item.MCPErrorCount, &item.MCPToolErrorCount, &item.ExecutionRequestID); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// GetUserRequestLog resolves only the numeric execution ID using exact equality.
// A missing execution leaves that ID nil and preserves its intended request ID;
// a missing entry returns pgx.ErrNoRows without reading any execution payload.
func (s *Store) GetUserRequestLog(ctx context.Context, id int64) (model.UserRequestLog, *int64, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT e.id, e.request_id, e.created_at, e.operation, e.compat_format, e.method,
		       e.path, e.client_ip, e.auth_type, e.api_token_id, e.token_name,
		       e.http_status, e.completion, e.latency_ms, e.mcp_error_count,
		       e.mcp_tool_error_count, e.execution_request_id, s.id
		FROM user_request_logs e
		LEFT JOIN search_requests s ON s.request_id=e.execution_request_id
		WHERE e.id=$1
	`, id)
	var item model.UserRequestLog
	var executionID *int64
	if err := row.Scan(&item.ID, &item.RequestID, &item.CreatedAt, &item.Operation,
		&item.CompatFormat, &item.Method, &item.Path, &item.ClientIP, &item.AuthType,
		&item.APITokenID, &item.TokenName, &item.HTTPStatus, &item.Completion,
		&item.LatencyMS, &item.MCPErrorCount, &item.MCPToolErrorCount,
		&item.ExecutionRequestID, &executionID); err != nil {
		return model.UserRequestLog{}, nil, err
	}
	return item, executionID, nil
}
