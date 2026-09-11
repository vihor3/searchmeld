package model

import "time"

const (
	UserRequestLogIDMaxBytes        = 256
	UserRequestLogMethodMaxBytes    = 16
	UserRequestLogPathMaxBytes      = 2048
	UserRequestLogTokenNameMaxBytes = 256
	UserRequestLogClientIPMaxBytes  = 64
)

// UserRequestLogInput contains request-time metadata, independent of execution
// accounting. Nullable identities and status remain explicit in management JSON.
type UserRequestLogInput struct {
	RequestID          string    `json:"request_id"`
	CreatedAt          time.Time `json:"created_at"`
	Operation          string    `json:"operation"`
	CompatFormat       string    `json:"compat_format"`
	Method             string    `json:"method"`
	Path               string    `json:"path"`
	ClientIP           string    `json:"client_ip"`
	AuthType           string    `json:"auth_type"`
	APITokenID         *int64    `json:"api_token_id"`
	TokenName          string    `json:"token_name"`
	HTTPStatus         *int      `json:"http_status"`
	Completion         string    `json:"completion"`
	LatencyMS          int64     `json:"latency_ms"`
	MCPErrorCount      int       `json:"mcp_error_count"`
	MCPToolErrorCount  int       `json:"mcp_tool_error_count"`
	ExecutionRequestID *string   `json:"execution_request_id"`
}

// UserRequestLog retains the observed Token snapshot even after Token deletion.
type UserRequestLog struct {
	UserRequestLogInput
	ID int64 `json:"id"`
}
