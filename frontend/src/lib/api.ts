/* 統一的後台 API 呼叫封裝：自動附 Bearer、401 時清除密鑰導回登入頁 */
import { adminKey } from '@/lib/admin-key'

const ADMIN_API = '/admin/api'

/* 統一取出錯誤訊息，供 toast 顯示 */
export function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

/* 驗證密鑰是否有效（登入頁與 AuthGuard 使用）；僅 401/403 視為密鑰無效，429/5xx 與其他非 2xx 向上拋出避免後端重啟誤踢出 */
export async function verifyKey(key: string): Promise<boolean> {
  const r = await fetch(`${ADMIN_API}/verify`, {
    headers: key ? { Authorization: `Bearer ${key}` } : {},
  })
  if (r.ok) return true
  if (r.status === 401 || r.status === 403) return false
  if (r.status === 429) {
    throw new Error('請求過於頻繁，請稍後再試')
  }
  const d = await r.json().catch(() => ({}) as { detail?: string })
  throw new Error((d as { detail?: string }).detail || `後端服務異常（${r.status}）`)
}

export async function api<T>(method: 'GET' | 'POST' | 'PUT' | 'DELETE', path: string, body?: unknown): Promise<T> {
  const key = await adminKey.get()
  const r = await fetch(ADMIN_API + path, {
    method,
    headers: {
      ...(body != null && { 'Content-Type': 'application/json' }),
      Authorization: `Bearer ${key}`,
    },
    ...(body != null && { body: JSON.stringify(body) }),
  })
  if (r.status === 401) {
    adminKey.clear()
    location.href = '/admin/login'
    throw new Error('登入已過期，請重新登入')
  }
  if (!r.ok) {
    const d = await r.json().catch(() => ({}) as { detail?: string })
    throw new Error((d as { detail?: string }).detail || String(r.status))
  }
  return r.json() as Promise<T>
}
