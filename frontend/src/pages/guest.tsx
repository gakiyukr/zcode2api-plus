/* 訪客提交頁：輸入邀請碼 → OAuth 授權 → 貼回調地址 → 後端實測通過才入池。
 *
 * 與後台分離：本頁不走 adminKey，只用邀請碼，且不顯示任何帳號資訊。
 * 設計上刻意不提供「貼上令牌」入口——OAuth 授權能證明提交者確實持有該帳號，
 * 而一串貼上的 JWT 證明不了任何事。 */
import { ExternalLink, Layers, Loader2, ShieldCheck } from 'lucide-react'
import { useEffect, useState, type FormEvent } from 'react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'

const GUEST_API = '/guest/api'

/* 訪客端錯誤取出：後端統一用 FastAPI 形態的 {"detail": ...} */
async function guestError(r: Response): Promise<string> {
  const d = (await r.json().catch(() => ({}))) as { detail?: string }
  return d.detail || `請求失敗（${r.status}）`
}

export function GuestPage() {
  const [enabled, setEnabled] = useState<boolean | null>(null)
  const [invite, setInvite] = useState('')
  const [authorizeURL, setAuthorizeURL] = useState('')
  const [flowID, setFlowID] = useState('')
  const [callbackURL, setCallbackURL] = useState('')
  const [busy, setBusy] = useState(false)
  const [done, setDone] = useState(false)

  /* 進場先問入口是否開放：關閉時不該讓訪客白填一輪 */
  useEffect(() => {
    let alive = true
    void (async () => {
      try {
        const r = await fetch(`${GUEST_API}/info`)
        const d = (await r.json()) as { enabled?: boolean }
        if (alive) setEnabled(Boolean(d.enabled))
      } catch {
        if (alive) setEnabled(false)
      }
    })()
    return () => {
      alive = false
    }
  }, [])

  async function start() {
    const code = invite.trim()
    if (!code || busy) return
    setBusy(true)
    try {
      const r = await fetch(`${GUEST_API}/start`, {
        method: 'POST',
        headers: { 'x-invite-code': code },
      })
      if (!r.ok) {
        toast.error(await guestError(r))
        return
      }
      const d = (await r.json()) as { flow_id: string; authorize_url: string }
      setFlowID(d.flow_id)
      setAuthorizeURL(d.authorize_url)
      // 直接開新視窗，省去手動複製；被攔截時下方仍顯示可複製的連結
      window.open(d.authorize_url, '_blank', 'noopener,noreferrer')
    } catch {
      toast.error('連線失敗')
    } finally {
      setBusy(false)
    }
  }

  async function complete() {
    const cb = callbackURL.trim()
    if (!cb || busy) return
    setBusy(true)
    try {
      const r = await fetch(`${GUEST_API}/complete`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'x-invite-code': invite.trim(),
        },
        body: JSON.stringify({ flow_id: flowID, callback_url: cb }),
      })
      if (!r.ok) {
        toast.error(await guestError(r))
        return
      }
      setDone(true)
    } catch {
      toast.error('連線失敗')
    } finally {
      setBusy(false)
    }
  }

  function onStart(e: FormEvent) {
    e.preventDefault()
    void start()
  }

  function onComplete(e: FormEvent) {
    e.preventDefault()
    void complete()
  }

  if (enabled === null) {
    return (
      <div className="flex min-h-svh items-center justify-center bg-muted/40">
        <Loader2 className="size-6 animate-spin text-muted-foreground" aria-label="載入中" />
      </div>
    )
  }

  if (!enabled) {
    return (
      <div className="flex min-h-svh items-center justify-center bg-muted/40 p-4">
        <Card className="w-full max-w-sm">
          <CardContent className="flex flex-col items-center gap-4 pt-2 text-center">
            <div className="flex size-11 items-center justify-center rounded-xl bg-muted text-muted-foreground">
              <Layers className="size-5" />
            </div>
            <div className="space-y-1">
              <h1 className="text-lg font-semibold tracking-tight">未開放</h1>
              <p className="text-sm text-muted-foreground">站點未開啟訪客提交，請聯絡管理員</p>
            </div>
          </CardContent>
        </Card>
      </div>
    )
  }

  if (done) {
    return (
      <div className="flex min-h-svh items-center justify-center bg-muted/40 p-4">
        <Card className="w-full max-w-sm">
          <CardContent className="flex flex-col items-center gap-4 pt-2 text-center">
            <div className="flex size-11 items-center justify-center rounded-xl bg-primary text-primary-foreground">
              <ShieldCheck className="size-5" />
            </div>
            <div className="space-y-1">
              <h1 className="text-lg font-semibold tracking-tight">提交成功</h1>
              <p className="text-sm text-muted-foreground">
                帳號已通過實測並加入池中，感謝你的貢獻
              </p>
            </div>
          </CardContent>
        </Card>
      </div>
    )
  }

  return (
    <div className="flex min-h-svh items-center justify-center bg-muted/40 p-4">
      <Card className="w-full max-w-md">
        <CardContent className="flex flex-col gap-6">
          <div className="flex flex-col items-center gap-4 pt-2 text-center">
            <div className="flex size-11 items-center justify-center rounded-xl bg-primary text-primary-foreground">
              <Layers className="size-5" />
            </div>
            <div className="space-y-1">
              <h1 className="text-lg font-semibold tracking-tight">提交帳號</h1>
              <p className="text-sm text-muted-foreground">
                透過 Z.AI 授權登入，通過實測後自動加入共享池
              </p>
            </div>
          </div>

          {!authorizeURL ? (
            <form className="flex flex-col gap-3" onSubmit={onStart}>
              <Input
                type="password"
                placeholder="邀請碼"
                autoFocus
                value={invite}
                onChange={(e) => setInvite(e.target.value)}
              />
              <Button type="submit" className="w-full" disabled={busy || !invite.trim()}>
                {busy ? <Loader2 className="size-4 animate-spin" /> : null}
                開始授權
              </Button>
              <p className="text-xs text-muted-foreground">
                授權僅用於驗證你確實持有該帳號；通過一次真實請求實測後才會入池。
              </p>
            </form>
          ) : (
            <div className="flex flex-col gap-4">
              <div className="flex flex-col gap-2">
                <p className="text-sm font-medium">1. 在開啟的頁面完成授權</p>
                <a
                  className="inline-flex items-center gap-1 text-sm text-primary underline-offset-4 hover:underline"
                  href={authorizeURL}
                  target="_blank"
                  rel="noopener noreferrer"
                >
                  若未自動開啟，點此前往授權頁
                  <ExternalLink className="size-3.5" />
                </a>
              </div>
              <form className="flex flex-col gap-2" onSubmit={onComplete}>
                <p className="text-sm font-medium">2. 貼上授權完成後的頁面地址</p>
                <Input
                  placeholder="https://zcode.z.ai/app/oauth/login?code=..."
                  value={callbackURL}
                  onChange={(e) => setCallbackURL(e.target.value)}
                />
                <Button type="submit" className="w-full" disabled={busy || !callbackURL.trim()}>
                  {busy ? <Loader2 className="size-4 animate-spin" /> : null}
                  提交並實測
                </Button>
              </form>
              <p className="text-xs text-muted-foreground">
                實測會發起一次最小的真實請求；失敗的帳號不會入池。
              </p>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
