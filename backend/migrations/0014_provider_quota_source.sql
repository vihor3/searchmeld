ALTER TABLE provider_keys ADD COLUMN IF NOT EXISTS official_quota_source TEXT NOT NULL DEFAULT '';
ALTER TABLE provider_keys ADD COLUMN IF NOT EXISTS official_quota_confidence TEXT NOT NULL DEFAULT '';
