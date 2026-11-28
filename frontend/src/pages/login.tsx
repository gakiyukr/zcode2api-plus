/* 登入頁：驗證後台密碼並存入本機加密儲存；已有有效密鑰時直接進儀表板 */
import { Layers, Loader2 } from 'lucide-react'
import { useEffect, useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { adminKey } from '@/lib/admin-key'
import { verifyKey } from '@/lib/api'

export function LoginPage() {
  const navigate = useNavigate()
  const [key, setKey] = useState('')
  const [busy, setBusy] = useState(false)
  const [checking, setChecking] = useState(true)

  /* 進場檢查：既有 session 有效就免登入 */
  useEffect(() => {
    let alive = true
    void (async () => {
      const stored = await adminKey.get()
      if (stored && (await verifyKey(stored).catch(() => false))) {
        if (!alive) return
        navigate('/admin/dashboard', { replace: true })
        return
      }
      if (alive) setChecking(false)
    })()
    return () => {
      alive = false
    }
  }, [navigate])

  async function submit() {
    const value = key.trim()
    if (!value || busy) return
    setBusy(true)
    try {
      if (await verifyKey(value)) {
        await adminKey.set(value)
        navigate('/admin/dashboard', { replace: true })
      } else {
        toast.error('密碼無效')
      }
    } catch (e) {
      toast.error(e instanceof TypeError ? '連線失敗' : e instanceof Error && e.message ? e.message : '連線失敗')
    } finally {
      setBusy(false)
    }
  }

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    void submit()
  }

  if (checking) {
    return (
      <div className="flex min-h-svh items-center justify-center bg-muted/40">
        <Loader2 className="size-6 animate-spin text-muted-foreground" aria-label="檢查登入狀態" />
      </div>
    )
  }

  return (
    <div className="flex min-h-svh items-center justify-center bg-muted/40 p-4">
      <Card className="w-full max-w-sm">
        <CardContent className="flex flex-col gap-6">
          <div className="flex flex-col items-center gap-4 pt-2 text-center">
            <div className="flex size-11 items-center justify-center rounded-xl bg-primary text-primary-foreground">
              <Layers className="size-5" />
            </div>
            <div className="space-y-1">
              <h1 className="text-lg font-semibold tracking-tight">後台管理</h1>
              <p className="text-sm text-muted-foreground">zcode2api-plus · 請輸入後台密碼以繼續</p>
            </div>
          </div>
          <form className="flex flex-col gap-3" onSubmit={onSubmit}>
            <Input
              type="password"
              placeholder="後台密碼"
              autoFocus
              value={key}
              onChange={(e) => setKey(e.target.value)}
            />
            <Button type="submit" className="w-full" disabled={busy || !key.trim()}>
              {busy ? <Loader2 className="size-4 animate-spin" /> : null}
              繼續
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}
