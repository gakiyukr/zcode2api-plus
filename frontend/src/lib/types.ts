/* 對照後端 public_view（app/models.py）與 /admin/api 回應的型別定義 */

export interface QuotaWindow {
  total?: number | null
  used?: number | null
  remaining?: number | null
  available?: number | null
  period?: string | null
  period_start?: number | string | null
  period_end?: number | string | null
  expires_at?: number | string | null
  model: string
  plan_name: string
  plan_is_trial: boolean
}

export interface TotalTokens {
  input: number
  output: number
  cache_creation: number
  cache_read: number
}

export type AccountStatus = 'active' | 'exhausted' | 'cooling' | 'invalid' | 'disabled'

export interface Account {
  id: string
  name: string
  email: string | null
  provider: string
  mode: 'jwt' | 'apiKey'
  token_masked: string
  enabled: boolean
  status: AccountStatus
  quota: Record<string, QuotaWindow>
  exhausted_models: string[]
  disabled_models: string[]
  plan: Record<string, unknown>
  plans: Record<string, unknown>[]
  plan_name: string
  plan_is_trial: boolean
  use_count: number
  fail_count: number
  total_tokens: TotalTokens
  last_used_at: number | null
  last_checked_at: number | null
  cooling_until: number | null
  last_error: string | null
  proxy_url: string | null
  proxy_id: string | null
  created_at: number
}

export interface Stats {
  total: number
  active: number
  exhausted: number
  cooling: number
  invalid: number
  disabled: number
  calls: number
  fail: number
  tokens_in: number
  tokens_out: number
  tokens_cache: number
}

export interface ProxyProfile {
  id: string
  name: string
  url: string
  enabled: boolean
}

export interface AccountsResponse {
  accounts: Account[]
  stats: Stats
  providers: string[]
  models: string[]
  proxies: ProxyProfile[]
  ts: number
}

export interface StatusResponse {
  providers: string[]
  gateway_key_set: boolean
  quota_pool: Record<string, number>
}

export interface SettingsResponse {
  admin_key: string
  gateway_key: string
  quota_refresh_interval: number
  rate_limit_threshold: number
}

export interface EgressInfo {
  ip: string
  asn: string
  operator: string
  country: string
  country_code: string
  ok?: boolean
  source?: string
  latency_ms?: number
}

export interface MonitorResponse {
  ts: number
  uptime_sec: number
  system: {
    cpu_count: number
    load_1m: number | null
    memory: { total_mb: number | null; available_mb: number | null; used_percent: number | null }
  }
  requests: { total: number; errors: number; success_rate: number | null; average_qps: number }
  accounts: { total: number; active: number; cooling: number; exhausted: number; invalid: number; disabled: number }
  services: { name: string; status: string; detail: string }[]
}

export interface UsageRankingRow {
  name: string
  provider: string
  requests: number
  errors: number
  tokens: number
}

export interface UsageResponse {
  ts: number
  window: string
  summary: Stats
  ranking: UsageRankingRow[]
}

export interface LoginStartResponse {
  flow_id: string
  authorize_url: string
}

export interface LoginCompleteResponse {
  status: 'ready' | 'failed' | 'pending'
  message?: string
  account?: Account
}

export interface CaptchaConfig {
  sceneId?: string
  region?: string
  prefix?: string
  enabled?: boolean
}

export interface RefreshSummary {
  ok: number
  fail: number
}

export const STATUS_LABEL: Record<AccountStatus, string> = {
  active: '正常',
  exhausted: '用完',
  cooling: '限流',
  invalid: '異常',
  disabled: '停用',
}

/* 儀表板用的完整狀態標籤與配色 */
export const STATUS_LABEL_LONG: Record<AccountStatus, string> = {
  active: '正常',
  exhausted: '額度用完',
  cooling: '冷卻中',
  invalid: '憑證異常',
  disabled: '已停用',
}

export const STATUS_COLOR: Record<AccountStatus, string> = {
  active: '#22c55e',
  exhausted: '#a855f7',
  cooling: '#f59e0b',
  invalid: '#ef4444',
  disabled: '#94a3b8',
}
