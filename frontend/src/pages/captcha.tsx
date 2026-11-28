/* 驗證中心頁：載入阿里雲驗證 SDK，於真實瀏覽器完成無痕驗證並提交結果供 JWT 請求複用 */
import { useEffect, useRef, useState } from 'react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { api, errMsg } from '@/lib/api'
import type { CaptchaConfig } from '@/lib/types'

const SDK_URL = 'https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js'

interface AliyunCaptchaInstance {
  startTracelessVerification?: () => void
  show?: () => void
}

interface AliyunCaptchaOptions {
  SceneId?: string
  mode: string
  region?: string
  prefix?: string
  element: string
  button: string
  captchaLogoImg: string
  showErrorTip: boolean
  getInstance: (inst: AliyunCaptchaInstance | undefined) => void
  success: (param: string) => void
  fail: (err: { message?: string } | unknown) => void
  onError: (err: { message?: string } | unknown) => void
}

declare global {
  interface Window {
    initAliyunCaptcha?: (options: AliyunCaptchaOptions) => void
  }
}

/* SDK 以 selector 綁定 DOM，故此頁固定使用 #cap 與 #btn 兩個 id；單例 Promise 防重複插標籤 */
let sdkLoading: Promise<void> | null = null

function loadSdk(): Promise<void> {
  if (window.initAliyunCaptcha) return Promise.resolve()
  if (sdkLoading) return sdkLoading
  sdkLoading = new Promise<void>((resolve, reject) => {
    const s = document.createElement('script')
    s.src = SDK_URL
    s.async = true
    s.onload = () => resolve()
    s.onerror = () => {
      sdkLoading = null
      reject(new Error('阿里雲驗證 SDK 載入失敗，請檢查網路後重新整理頁面。'))
    }
    document.head.appendChild(s)
  })
  return sdkLoading
}

function errText(err: { message?: string } | unknown): string {
  const m = (err as { message?: string })?.message
  return m || (err ? JSON.stringify(err) : 'unknown')
}

export function CaptchaPage() {
  const [status, setStatus] = useState('正在載入驗證配置…')
  const [hint, setHint] = useState('')
  const [ready, setReady] = useState(false)
  const [started, setStarted] = useState(false)
  const cfgRef = useRef<CaptchaConfig | null>(null)
  const instanceRef = useRef<AliyunCaptchaInstance | null>(null)
  const aliveRef = useRef(true)
  const startedRef = useRef(false)
  const attemptsRef = useRef(0)

  useEffect(() => {
    aliveRef.current = true
    void (async () => {
      try {
        await loadSdk()
      } catch (e) {
        if (aliveRef.current) setStatus(errMsg(e))
        return
      }
      try {
        const cfg = await api<CaptchaConfig>('GET', '/captcha/config')
        cfgRef.current = cfg
        if (!aliveRef.current) return
        setReady(true)
        setStatus(`驗證配置：scene=${cfg.sceneId || '-'} · region=${cfg.region || '-'} · ${cfg.enabled ? '已啟用' : '未啟用'}`)
        setHint('點擊「開始驗證」，將在真實瀏覽器中完成驗證；如果出現滑塊，請在彈窗中手動完成。')
      } catch (e) {
        if (aliveRef.current) setStatus('載入驗證配置失敗：' + errMsg(e))
      }
    })()
    return () => {
      aliveRef.current = false
      instanceRef.current = null
    }
  }, [])

  function reset() {
    startedRef.current = false
    if (aliveRef.current) setStarted(false)
  }

  function start() {
    const cfg = cfgRef.current
    if (!cfg || !window.initAliyunCaptcha || startedRef.current) return
    startedRef.current = true
    attemptsRef.current = 0
    setStarted(true)
    setHint('無痕驗證進行中：通常數秒內自動完成，期間不會出現彈窗；僅當風控要求二次驗證時才會彈出滑塊，請在彈窗中手動完成。')

    if (instanceRef.current) {
      try {
        const method = instanceRef.current.startTracelessVerification ?? instanceRef.current.show
        method?.call(instanceRef.current)
        return
      } catch {
        instanceRef.current = null
      }
    }

    window.initAliyunCaptcha({
      SceneId: cfg.sceneId,
      mode: 'popup',
      region: cfg.region,
      prefix: cfg.prefix,
      element: '#cap',
      button: '#btn',
      captchaLogoImg: '',
      showErrorTip: false,
      getInstance: (inst) => {
        /* SDK 驗證失敗後會銷毀重建實例，並以 undefined 回呼本函數，必須防護 */
        if (!aliveRef.current) return
        if (!inst) {
          instanceRef.current = null
          return
        }
        instanceRef.current = inst
        if (attemptsRef.current >= 3) {
          toast.error('多次重試仍未通過，請稍後再點「開始驗證」')
          setHint('驗證多次未通過。可稍後重試；若持續失敗，請檢查網路或更換出口 IP。')
          reset()
          return
        }
        attemptsRef.current++
        try {
          const method = inst.startTracelessVerification ?? inst.show
          method?.call(inst)
        } catch (e) {
          toast.error('啟動驗證失敗：' + errMsg(e))
          reset()
        }
      },
      success: async (param) => {
        try {
          await api('POST', '/captcha/submit', { verify_param: param })
          if (aliveRef.current) {
            toast.success('驗證結果已提交，約 45 秒內供 JWT 請求使用')
            setHint('已提交。驗證碼是短期結果，過期後請重新完成驗證。')
          }
        } catch (e) {
          if (aliveRef.current) {
            toast.error('提交失敗：' + errMsg(e))
            setHint('驗證已通過但提交失敗，請重新點擊「開始驗證」。')
          }
        }
        reset()
      },
      fail: (err) => {
        if (aliveRef.current) {
          toast.error('驗證失敗：' + errText(err))
          setHint('驗證未通過，SDK 會自動重試；若長時間無結果，可再次點擊「開始驗證」。')
        }
        reset()
      },
      onError: (err) => {
        if (aliveRef.current) {
          toast.error('驗證初始化失敗：' + errText(err))
        }
        reset()
      },
    })
  }

  return (
    <div className="mx-auto flex w-full max-w-2xl flex-col gap-6">
      {/* 頁首 */}
      <div>
        <h1 className="text-xl font-semibold tracking-tight">人機驗證</h1>
        <p className="text-sm text-muted-foreground">
          自動求解被風控攔截時，在真實瀏覽器手動完成阿里雲無痕驗證，結果供 JWT 帳號請求複用
        </p>
      </div>

      <Card>
        <CardContent className="flex flex-col gap-4">
          <p className="text-xs text-muted-foreground">{status}</p>
          <div id="cap" />
          <Button id="btn" className="h-10 w-full" disabled={!ready || started} onClick={start}>
            開始驗證
          </Button>
          {hint ? <p className="text-xs leading-relaxed text-muted-foreground">{hint}</p> : null}
        </CardContent>
      </Card>
    </div>
  )
}
