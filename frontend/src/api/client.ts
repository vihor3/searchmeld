import { useSessionStore } from '../stores/session'

const API_BASE = import.meta.env.VITE_API_BASE || ''

export interface AdminProfile {
  username: string
}

export class ApiError extends Error {
  /** Keep HTTP status available for auth decisions even when the error body is absent. */
  constructor(public readonly status: number, message: string) {
    super(message)
    this.name = 'ApiError'
  }
}

export interface UsageSummary {
  requests_total: number
  requests_success: number
  requests_failed: number
  cache_hits: number
  results_total: number
  average_latency_ms: number
}

export interface UsageUnitSummary {
  provider_name: string
  unit: string
  quantity_total: number
  cost_usd_total: number
}

export interface BillingSummary {
  days: number
  units: UsageUnitSummary[]
}

export interface ProviderHealth {
  provider_name: string
  display_name: string
  enabled: boolean
  status: string
  available_keys: number
  total_keys: number
  exhausted_keys: number
  disabled_keys: number
  cooling_keys: number
  requests_total: number
  requests_failed: number
  success_rate: number
  last_error: string
  last_checked_at: string
  window_minutes: number
}

export interface GatewayMetrics {
  usage: UsageSummary
  provider_health: ProviderHealth[]
  billing: BillingSummary
}

export interface UsageSeriesPoint {
  date: string
  requests_total: number
  requests_success: number
  requests_failed: number
  cache_hits: number
  results_total: number
  average_latency_ms: number
}

export interface UsageSeries {
  range?: string
  granularity?: 'hour' | 'day' | string
  days: number
  points: UsageSeriesPoint[]
}

export type DashboardRangeKey = '24h' | 'today' | '7d' | '14d' | '30d'

export interface DashboardRangeMeta {
  range: DashboardRangeKey | string
  label: string
  granularity: 'hour' | 'day' | string
  segment_minutes: number
  segments: number
  billing_days: number
}

export interface ProviderUsagePoint {
  provider_name: string
  display_name: string
  requests_total: number
}

export interface HealthSegmentPoint {
  status: 'ok' | 'degraded' | 'down' | 'off' | string
  success: number
  failed: number
  total: number
}

export interface HealthSegmentSeries {
  provider_name: string
  display_name: string
  status: string
  available_keys: number
  total_keys: number
  success_rate: number
  uptime_percent: number
  segments: HealthSegmentPoint[]
  segment_minutes: number
}

export interface AuditLog {
  id: number
  request_id: string
  actor: string
  action: string
  resource_type: string
  resource_id: string
  ip_address: string
  metadata: Record<string, unknown>
  created_at: string
}

export interface ProviderConfig {
  id: number
  name: string
  display_name: string
  base_url: string
  enabled: boolean
  priority: number
  weight: number
  timeout_ms: number
  settings?: Record<string, unknown>
  available_keys?: number
}

export interface ProviderKey {
  id: number
  provider_id: number
  provider_name: string
  alias: string
  key_hint: string
  key?: string
  exa_api_key_id?: string
  exa_service_key_hint?: string
  status: string
  weight: number
  rpm_limit: number
  daily_quota: number
  monthly_quota: number
  max_concurrency: number
  current_failures: number
  total_successes: number
  total_failures: number
  daily_used: number
  monthly_used: number
  official_quota_status: string
  official_quota_message: string
  official_quota_source: string
  official_quota_confidence: string
  official_quota_unit: string
  official_quota_balance?: number
  official_quota_balance_usd?: number
  official_quota_used_usd?: number
  official_quota_total_quantity?: number
  official_quota_account_id?: string
  official_quota_checked_at?: string
  cooldown_until?: string
  last_used_at?: string
  created_at: string
  updated_at: string
}

export interface OfficialQuotaResult {
  provider: string
  alias: string
  supported: boolean
  status: string
  message?: string
  source?: string
  confidence?: string
  unit?: string
  balance?: number
  balance_cents?: number
  balance_usd?: number
  total_cost_usd?: number
  total_quantity?: number
  api_key_id?: string
  api_key_name?: string
  account_id?: string
  period?: { start?: string; end?: string }
  breakdown?: Record<string, unknown>[]
  raw_text?: string
  fetched_at: string
}

export interface ApiToken {
  id: number
  name: string
  token_prefix: string
  token?: string
  scopes: string[]
  allowed_providers: string[]
  status: string
  rate_limit_per_min: number
  daily_quota: number
  monthly_quota: number
  last_used_at?: string
  usage_count: number
  created_at: string
  updated_at: string
}

export interface AdminAPIKey {
  key?: string
  key_prefix: string
  created_at?: string
  updated_at?: string
}

export interface RuntimeSettings {
  default_mode: string
  default_providers: string[]
  default_limit: number
  default_dedupe: boolean
  request_timeout_ms: number
  cache_enabled: boolean
  cache_ttl_seconds: number
  cache_max_results: number
  compat_tavily_enabled: boolean
  compat_serper_enabled: boolean
  compat_openai_enabled: boolean
  api_auth_required: boolean
  allow_private_extract_targets: boolean
  provider_health_window_minutes: number
  provider_routing_strategy: string
  log_retention_days: number
  search_logs_limit: number
}

export interface SearchLog {
  id: number
  request_id: string
  operation: 'search' | 'extract' | string
  query: string
  mode: string
  compat_format: string
  providers: string[]
  cache_policy: string
  cache_hit: boolean
  result_count: number
  status: string
  error_message: string
  latency_ms: number
  request_json?: unknown
  response_json?: unknown
  created_at: string
}

export interface ProviderCallLog {
  provider_key_id: number
  provider_name: string
  key_alias: string
  attempt_index?: number
  will_retry?: boolean
  status: string
  error_type: string
  error_message: string
  latency_ms: number
  result_count: number
  cached: boolean
  usage?: Array<{ unit: string; quantity: number; cost_usd?: number; metadata?: Record<string, unknown> }>
}

/**
 * Read JSON with Cookie credentials, browser proof and no-store for admin paths.
 * Current-revision protected 401s request recovery before body parsing, then reject;
 * login/logout/me and stale responses do not trigger that recovery. Never replay
 * a failed request. HTTP failures retain status in ApiError even without JSON;
 * network errors and invalid success JSON propagate. Successful payload validation
 * belongs to the caller.
 */
export async function apiFetch<T>(path: string, options: RequestInit = {}): Promise<T> {
  const session = useSessionStore()
  const requestRevision = session.revision
  const { pathname } = new URL(path, window.location.origin)
  const adminRequest = pathname.startsWith('/api/admin/')
  const headers = new Headers(options.headers || {})
  headers.set('Content-Type', 'application/json')
  if (adminRequest) headers.set('X-SearchMeld-Admin', '1')
  const response = await fetch(`${API_BASE}${path}`, {
    ...options,
    headers,
    credentials: adminRequest ? 'include' : options.credentials,
    cache: adminRequest ? 'no-store' : options.cache
  })
  if (!response.ok) {
    if (response.status === 401 && adminRequest && requestRevision === session.revision &&
      !['/api/admin/login', '/api/admin/logout', '/api/admin/me'].includes(pathname)) {
      // Probe/navigation precedes error-body parsing; never replay the rejected request.
      const { recheckSession } = await import('../router')
      await recheckSession(requestRevision)
    }
    const payload: unknown = await response.json().catch(() => null)
    let message = response.statusText || `HTTP ${response.status}`
    if (payload && typeof payload === 'object' && 'error' in payload) {
      const detail = payload.error
      if (detail && typeof detail === 'object' && 'message' in detail && typeof detail.message === 'string' && detail.message) {
        message = detail.message
      }
    }
    throw new ApiError(response.status, message)
  }
  return response.json() as Promise<T>
}

export const api = {
  /** Post credentials for a server-issued Cookie; JSON returns expiry, not a bearer token. */
  login: (username: string, password: string) => apiFetch<{ expires_at: string }>('/api/admin/login', { method: 'POST', body: JSON.stringify({ username, password }) }),
  /**
   * Probe the current Cookie; only HTTP 401 means anonymous. Malformed profiles
   * and all other failures reject so callers retain an unknown, retryable state.
   */
  me: async (): Promise<AdminProfile | null> => {
    try {
      const payload = await apiFetch<unknown>('/api/admin/me')
      if (!payload || typeof payload !== 'object' || !('username' in payload) || typeof payload.username !== 'string' || !payload.username.trim()) {
        throw new Error('\u767b\u5f55\u72b6\u6001\u54cd\u5e94\u65e0\u6548\uff0c\u8bf7\u91cd\u8bd5')
      }
      return { username: payload.username }
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) return null
      throw error
    }
  },
  /** Request server-side Cookie-session revocation; leave failures, including 401, to the caller. */
  logout: () => apiFetch<{ status: string }>('/api/admin/logout', { method: 'POST' }),
  dashboard: (range: DashboardRangeKey | string = '14d') => apiFetch<{
    range?: DashboardRangeMeta
    usage: UsageSummary
    providers: ProviderConfig[]
    provider_health?: ProviderHealth[]
    billing?: BillingSummary
    usage_series?: UsageSeries
    provider_series?: ProviderUsagePoint[]
    health_series?: HealthSegmentSeries[]
  }>(`/api/admin/dashboard?range=${encodeURIComponent(range)}`),
  providers: () => apiFetch<{ providers: ProviderConfig[] }>('/api/admin/providers'),
  updateProvider: (provider: ProviderConfig) => apiFetch('/api/admin/providers/' + provider.name, { method: 'PATCH', body: JSON.stringify(provider) }),
  keys: () => apiFetch<{ keys: ProviderKey[] }>('/api/admin/keys'),
  createKey: (payload: Record<string, unknown>) => apiFetch<ProviderKey>('/api/admin/keys', { method: 'POST', body: JSON.stringify(payload) }),
  revealKey: (id: number) => apiFetch<{ id: number; provider_name: string; alias: string; key: string; key_hint: string; exa_service_key?: string }>('/api/admin/keys/' + id + '/secret'),
  updateKey: (id: number, payload: Record<string, unknown>) => apiFetch<ProviderKey>('/api/admin/keys/' + id, { method: 'PATCH', body: JSON.stringify(payload) }),
  deleteKey: (id: number) => apiFetch('/api/admin/keys/' + id, { method: 'DELETE' }),
  testKey: (id: number, payload: Record<string, unknown>) => apiFetch('/api/admin/keys/' + id + '/test', { method: 'POST', body: JSON.stringify(payload) }),
  queryKeyQuota: (id: number, payload: Record<string, unknown> = {}) => apiFetch<OfficialQuotaResult>('/api/admin/keys/' + id + '/quota', { method: 'POST', body: JSON.stringify(payload) }),
  tokens: () => apiFetch<{ tokens: ApiToken[] }>('/api/admin/tokens'),
  createToken: (payload: Record<string, unknown>) => apiFetch<{ token: ApiToken; raw_token: string }>('/api/admin/tokens', { method: 'POST', body: JSON.stringify(payload) }),
  revealToken: (id: number) => apiFetch<{ id: number; name: string; token: string; token_prefix: string }>('/api/admin/tokens/' + id + '/secret'),
  updateToken: (id: number, payload: Record<string, unknown>) => apiFetch('/api/admin/tokens/' + id, { method: 'PATCH', body: JSON.stringify(payload) }),
  deleteToken: (id: number) => apiFetch('/api/admin/tokens/' + id, { method: 'DELETE' }),
  settings: () => apiFetch<RuntimeSettings>('/api/admin/settings'),
  updateSettings: (payload: RuntimeSettings) => apiFetch<RuntimeSettings>('/api/admin/settings', { method: 'PUT', body: JSON.stringify(payload) }),
  adminAPIKey: () => apiFetch<AdminAPIKey>('/api/admin/settings/admin-api-key'),
  rotateAdminAPIKey: () => apiFetch<AdminAPIKey>('/api/admin/settings/admin-api-key', { method: 'POST' }),
  logs: (limit?: number) => apiFetch<{ logs: SearchLog[] }>(limit == null ? '/api/admin/logs' : `/api/admin/logs?limit=${Math.max(1, Math.min(limit, 1000))}`),
  logDetail: (id: number) => apiFetch<{ log: SearchLog; calls: ProviderCallLog[] }>('/api/admin/logs/' + id),
  usageSummary: () => apiFetch<UsageSummary>('/api/admin/usage/summary'),
  billingSummary: (days = 30) => apiFetch<BillingSummary>(`/api/admin/usage/billing?days=${days}`),
  providerHealth: () => apiFetch<{ providers: ProviderHealth[] }>('/api/admin/providers/health'),
  metrics: () => apiFetch<GatewayMetrics>('/api/admin/metrics'),
  auditLogs: () => apiFetch<{ logs: AuditLog[] }>('/api/admin/audit-logs?limit=100'),
  playgroundSearch: (payload: Record<string, unknown>) => apiFetch('/api/admin/playground/search', { method: 'POST', body: JSON.stringify(payload) })
}
