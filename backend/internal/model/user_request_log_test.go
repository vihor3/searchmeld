package model

import (
	"encoding/json"
	"testing"
	"time"
)

// TestUserRequestLogJSONNullability protects the management wire contract from
// omitted unknown fields and loss of the embedded metadata projection.
func TestUserRequestLogJSONNullability(t *testing.T) {
	createdAt := time.Date(2026, time.September, 11, 8, 0, 0, 0, time.UTC)
	log := UserRequestLog{
		ID:                  7,
		UserRequestLogInput: UserRequestLogInput{
			RequestID:  "entry-wire",
			CreatedAt:  createdAt,
			AuthType:   "unknown",
			Completion: "interrupted",
		},
	}
	for _, populated := range []bool{false, true} {
		if populated {
			tokenID, status, executionID := int64(13), 200, "entry-wire:tool"
			log.APITokenID = &tokenID
			log.HTTPStatus = &status
			log.ExecutionRequestID = &executionID
		}
		payload, err := json.Marshal(log)
		if err != nil {
			t.Fatalf("marshal user request: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(payload, &fields); err != nil {
			t.Fatalf("decode user request: %v", err)
		}
		keys := []string{
			"id", "request_id", "created_at", "operation", "compat_format", "method",
			"path", "client_ip", "auth_type", "api_token_id", "token_name", "http_status",
			"completion", "latency_ms", "mcp_error_count", "mcp_tool_error_count", "execution_request_id",
		}
		if len(fields) != len(keys) {
			t.Fatalf("metadata JSON has %d fields, want %d", len(fields), len(keys))
		}
		for _, key := range keys {
			if _, exists := fields[key]; !exists {
				t.Fatalf("metadata JSON is missing %s", key)
			}
		}
		for _, key := range []string{"api_token_id", "http_status", "execution_request_id"} {
			if isNull := string(fields[key]) == "null"; isNull == populated {
				t.Fatalf("%s null=%v for populated=%v", key, isNull, populated)
			}
		}
		var decoded UserRequestLog
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatalf("decode typed user request: %v", err)
		}
		if decoded.ID != log.ID || decoded.RequestID != log.RequestID || !decoded.CreatedAt.Equal(createdAt) {
			t.Fatal("metadata JSON lost its entry identity or request start time")
		}
	}
}
