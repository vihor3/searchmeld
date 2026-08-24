ALTER TABLE search_requests
    ADD COLUMN IF NOT EXISTS operation TEXT NOT NULL DEFAULT 'search';

CREATE INDEX IF NOT EXISTS idx_search_requests_operation_created_at
    ON search_requests(operation, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_usage_daily_provider_key_date
    ON usage_daily(provider_key_id, usage_date DESC)
    WHERE provider_key_id IS NOT NULL;
