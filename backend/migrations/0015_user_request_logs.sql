CREATE TABLE IF NOT EXISTS user_request_logs (
    id BIGSERIAL PRIMARY KEY,
    request_id TEXT NOT NULL UNIQUE CHECK (octet_length(request_id) BETWEEN 1 AND 256),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    operation TEXT NOT NULL CHECK (operation IN ('search', 'extract', 'mcp')),
    compat_format TEXT NOT NULL CHECK (compat_format IN ('native', 'tavily', 'serper', 'openai')),
    method TEXT NOT NULL CHECK (octet_length(method) BETWEEN 1 AND 16),
    path TEXT NOT NULL CHECK (octet_length(path) BETWEEN 1 AND 2048),
    client_ip TEXT NOT NULL DEFAULT '' CHECK (octet_length(client_ip) <= 64),
    auth_type TEXT NOT NULL CHECK (auth_type IN ('unknown', 'anonymous', 'api_token', 'admin_key')),
    -- Request-time snapshots intentionally have no foreign keys.
    api_token_id BIGINT CHECK (api_token_id > 0),
    token_name TEXT NOT NULL DEFAULT '' CHECK (octet_length(token_name) <= 256),
    http_status INTEGER CHECK (http_status BETWEEN 100 AND 999),
    completion TEXT NOT NULL CHECK (completion IN ('completed', 'interrupted', 'canceled', 'write_error')),
    latency_ms BIGINT NOT NULL CHECK (latency_ms >= 0),
    mcp_error_count INTEGER NOT NULL DEFAULT 0 CHECK (mcp_error_count BETWEEN 0 AND 32),
    mcp_tool_error_count INTEGER NOT NULL DEFAULT 0 CHECK (mcp_tool_error_count BETWEEN 0 AND 32),
    execution_request_id TEXT CHECK (octet_length(execution_request_id) BETWEEN 1 AND 256)
);

CREATE INDEX IF NOT EXISTS idx_user_request_logs_created_at
    ON user_request_logs(created_at DESC, id DESC);
