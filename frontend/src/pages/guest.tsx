/* 訪客提交頁：輸入邀請碼 → 人機驗證 → OAuth 授權 → 貼回調地址 → 後端實測通過才入池。
 *
 * 與後台分離：本頁不走 adminKey，只用邀請碼，且不顯示任何帳號資訊。
 * 設計上刻意不提供「貼上令牌」入口——OAuth 授權能證明提交者確實持有該帳號，
 * 而一串貼上的 JWT 證明不了任何事。
 *
 * 人機驗證用自建的 Cap（PoW，無第三方）。兩步各驗一次：Cap token 是一次性的，
 * 第一步用過就失效，所以第二步要重新求解。未配置 Cap 時整段不渲染，後端也跳過。 */
import { ExternalLink, Layers, Loader2, ShieldCheck } from 'lucide-react'
import { useCallback, useEffect, useRef, useState, type DetailedHTMLProps, type FormEvent } from 'react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'

const GUEST_API = '/guest/api'

/* Cap 的 widget 是原生自訂元素（Web Component），需向 JSX 宣告才能使用。
   React 19 起 JSX 命名空间在 React.JSX 下（不再是 global JSX），故用 module 扩充。

   事件用 addEventListener 手動綁定而非 JSX 的 onsolve：實測 React 19 對自訂
   元素的 on* 屬性只寫入 props 而不註冊監聽器，導致 solve 事件永遠收不到、
   按鈕卡在禁用態。手動綁定不依賴框架對自訂元素的處理細節。 */
declare module 'react' {
  namespace JSX {
    interface IntrinsicElements {
      'cap-widget': DetailedHTMLProps<HTMLAttributes<HTMLElement>, HTMLElement> & {
        'data-cap-api-endpoint'?: string
      }
    }
  }
}

/* 載入 Cap 的 widget 腳本。固定版本而非 latest：
   widget 會隨上游更新，不固定版本等於讓外部決定本頁何時改變行為。 */
const CAP_WIDGET_SRC = 'https://cdn.jsdelivr.net/npm/cap-widget@0.1.57'
let capWidgetLoading: Promise<void> | null = null

function loadCapWidget(): Promise<void> {
  if (customElements.get('cap-widget')) return Promise.resolve()
  if (capWidgetLoading) return capWidgetLoading
  capWidgetLoading = new Promise<void>((resolve, reject) => {
    const s = document.createElement('script')
    s.type = 'module'
    s.src = CAP_WIDGET_SRC
    s.onload = () => resolve()
    s.onerror = () => {
      capWidgetLoading = null
      reject(new Error('人機驗證元件載入失敗'))
    }
    document.head.appendChild(s)
  })
  return capWidgetLoading
}

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

  /* 人機驗證：endpoint 由後端回傳（未配置時為空，整段不渲染） */
  const [capEndpoint, setCapEndpoint] = useState('')
  const [capReady, setCapReady] = useState(false)
  const [capToken, setCapToken] = useState('')
  const [capKey, setCapKey] = useState(0)
  /* widget 是命令式自訂元素，token 用 ref 讀取：事件回呼在渲染週期外觸發，
     靠 state 讀會拿到舊值。 */
  const tokenRef = useRef('')
  const widgetRef = useRef<HTMLElement | null>(null)

  /* 進場先問入口是否開放：關閉時不該讓訪客白填一輪 */
  useEffect(() => {
    let alive = true
    void (async () => {
      try {
        const r = await fetch(`${GUEST_API}/info`)
        const d = (await r.json()) as { enabled?: boolean; cap_endpoint?: string }
        if (!alive) return
        setEnabled(Boolean(d.enabled))
        setCapEndpoint(d.cap_endpoint || '')
      } catch {
        if (alive) setEnabled(false)
      }
    })()
    return () => {
      alive = false
    }
  }, [])

  /* 需要人機驗證時才載入腳本；未配置就不該拉外部資源 */
  useEffect(() => {
    if (!capEndpoint) return
    void loadCapWidget()
      .then(() => setCapReady(true))
      .catch(() => toast.error('人機驗證元件載入失敗，請檢查網路後重新整理'))
  }, [capEndpoint])

  /* 手動綁定 widget 事件。capKey 變化會重建元素，故依賴它重新綁定。 */
  useEffect(() => {
    const w = widgetRef.current
    if (!w) return
    const onSolve = (e: Event) => {
      const token = (e as CustomEvent<{ token?: string }>).detail?.token || ''
      tokenRef.current = token
      setCapToken(token)
    }
    const onError = () => {
      tokenRef.current = ''
      setCapToken('')
    }
    w.addEventListener('solve', onSolve)
    w.addEventListener('error', onError)
    return () => {
      w.removeEventListener('solve', onSolve)
      w.removeEventListener('error', onError)
    }
  }, [capReady, capKey])

  /* 用過一次就重置：Cap token 是一次性的，同一個不能提交第二次。
     capKey 遞增讓 React 重建元件，逼出一個全新的 token。 */
  const resetCaptcha = useCallback(() => {
    tokenRef.current = ''
    setCapToken('')
    setCapKey((k) => k + 1)
  }, [])

  async function start() {
    const code = invite.trim()
    if (!code || busy) return
    setBusy(true)
    try {
      const r = await fetch(`${GUEST_API}/start`, {
        method: 'POST',
        headers: { 'x-invite-code': code, 'x-cap-token': tokenRef.current },
      })
      if (!r.ok) {
        toast.error(await guestError(r))
        // token 已被消费（无论成败都算用过一次），失败后必须换新的
        resetCaptcha()
        return
      }
      const d = (await r.json()) as { flow_id: string; authorize_url: string }
      setFlowID(d.flow_id)
      setAuthorizeURL(d.authorize_url)
      // 不自動開窗：未經使用者點擊就跳轉到外部站台容易被當成彈窗廣告，
      // 也會在瀏覽器攔截時留下「什麼都沒發生」的困惑。改為明確的按鈕。
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
          'x-cap-token': tokenRef.current,
        },
        body: JSON.stringify({ flow_id: flowID, callback_url: cb }),
      })
      if (!r.ok) {
        toast.error(await guestError(r))
        resetCaptcha()
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

  /* Cap widget 需要在两步各渲染一次：token 是一次性的，第一步消费掉之后
     第二步必须重新求解。capKey 变化会重建元素，从而取一道新题。 */
  const captchaField = capEndpoint ? (
    capReady ? (
      <cap-widget key={capKey} ref={widgetRef} data-cap-api-endpoint={capEndpoint} />
    ) : (
      <div className="flex items-center gap-2 text-xs text-muted-foreground">
        <Loader2 className="size-3.5 animate-spin" />
        正在載入人機驗證…
      </div>
    )
  ) : null

  /* 配了人机验证就必须先拿到 token 才能提交 */
  const captchaBlocked = Boolean(capEndpoint) && !capToken

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
              {captchaField}
              <Button
                type="submit"
                className="w-full"
                disabled={busy || !invite.trim() || captchaBlocked}
              >
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
                <p className="text-sm font-medium">1. 前往授權頁完成登入</p>
                <Button asChild variant="outline" className="w-full">
                  <a href={authorizeURL} target="_blank" rel="noopener noreferrer">
                    開啟 Z.AI 授權頁
                    <ExternalLink className="size-3.5" />
                  </a>
                </Button>
              </div>
              <form className="flex flex-col gap-2" onSubmit={onComplete}>
                <p className="text-sm font-medium">2. 貼上授權完成後的頁面地址</p>
                <Input
                  placeholder="https://zcode.z.ai/app/oauth/login?code=..."
                  value={callbackURL}
                  onChange={(e) => setCallbackURL(e.target.value)}
                />
                {captchaField}
                <Button
                  type="submit"
                  className="w-full"
                  disabled={busy || !callbackURL.trim() || captchaBlocked}
                >
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
