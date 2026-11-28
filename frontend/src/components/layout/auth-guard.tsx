/* 進場驗證：本機已有密鑰才向後端驗證，未通過一律導回登入頁（對應舊 requireAdmin） */
import { Loader2 } from 'lucide-react'
import { useEffect, useState, type ReactNode } from 'react'
import { Navigate } from 'react-router-dom'
import { adminKey } from '@/lib/admin-key'
import { verifyKey } from '@/lib/api'

export function AuthGuard({ children }: { children: ReactNode }) {
  const [state, setState] = useState<'checking' | 'ok' | 'deny'>('checking')

  useEffect(() => {
    let alive = true
    void (async () => {
      const key = await adminKey.get()
      if (!key) {
        if (alive) setState('deny')
        return
      }
      let ok = false
      let invalidKey = false
      try {
        ok = await verifyKey(key)
        invalidKey = !ok
      } catch {
        ok = false
      }
      if (invalidKey) {
        // 密鑰明確驗證失敗（非限流或暫時斷網），主動清除避免重複無效請求
        adminKey.clear()
      }
      if (alive) setState(ok ? 'ok' : 'deny')
    })()
    return () => {
      alive = false
    }
  }, [])

  if (state === 'checking') {
    return (
      <div className="flex min-h-svh items-center justify-center">
        <Loader2 className="size-6 animate-spin text-muted-foreground" aria-label="驗證中" />
      </div>
    )
  }
  if (state === 'deny') return <Navigate to="/admin/login" replace />
  return <>{children}</>
}
