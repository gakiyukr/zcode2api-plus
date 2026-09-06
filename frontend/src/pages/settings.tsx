/* 系統設定頁：後台密碼、網關 API Key、額度刷新間隔與使用說明 */
import { useEffect, useState, type FormEvent } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Loader2 } from 'lucide-react'
import { toast } from 'sonner'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { adminKey } from '@/lib/admin-key'
import { api, errMsg } from '@/lib/api'
import type { SettingsResponse } from '@/lib/types'

export function SettingsPage() {
  const { data } = useQuery({
    queryKey: ['settings'],
    queryFn: () => api<SettingsResponse>('GET', '/settings'),
  })

  const [adminKeyInput, setAdminKeyInput] = useState('')
  const [gatewayKey, setGatewayKey] = useState('')
  const [quotaInterval, setQuotaInterval] = useState('60')
  const [rateLimitThreshold, setRateLimitThreshold] = useState('1')
  const [saving, setSaving] = useState(false)

  /* 載入完成後填入表單（僅在尚未編輯時同步） */
  useEffect(() => {
    if (!data) return
    setAdminKeyInput(data.admin_key || '')
    setGatewayKey(data.gateway_key || '')
    setQuotaInterval(String(data.quota_refresh_interval ?? 60))
    setRateLimitThreshold(String(data.rate_limit_threshold ?? 1))
  }, [data])

  async function save(e: FormEvent) {
    e.preventDefault()
    if (!adminKeyInput.trim()) {
      toast.error('後台密碼不能為空')
      return
    }
    if (!gatewayKey.trim()) {
      toast.error('網關 API Key 不能為空')
      return
    }
    const interval = parseInt(quotaInterval, 10)
    if (isNaN(interval) || interval < 0) {
      toast.error('刷新間隔必須是非負整數')
      return
    }
    const threshold = parseInt(rateLimitThreshold, 10)
    if (isNaN(threshold) || threshold < 0) {
      toast.error('限流降級閾值必須是非負整數')
      return
    }
    setSaving(true)
    try {
      await api('PUT', '/settings', {
        admin_key: adminKeyInput.trim(),
        gateway_key: gatewayKey.trim(),
        quota_refresh_interval: interval,
        rate_limit_threshold: threshold,
      })
      /* 同步本機儲存的密鑰，避免改密後被登出 */
      await adminKey.set(adminKeyInput.trim())
      toast.success('已儲存')
    } catch (err) {
      toast.error('儲存失敗：' + errMsg(err))
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="mx-auto flex w-full max-w-3xl flex-col gap-6">
      {/* 頁首 */}
      <div>
        <h1 className="text-xl font-semibold tracking-tight">系統設定</h1>
        <p className="text-sm text-muted-foreground">後台鑑權密鑰與網關存取控制</p>
      </div>

      {/* 鑑權設定 */}
      <Card>
        <CardContent className="flex flex-col gap-5">
          <div className="text-sm font-semibold">鑑權</div>
          <form className="flex flex-col gap-5" onSubmit={save}>
            <div className="flex flex-col gap-2">
              <Label htmlFor="set-admin-key">後台密碼</Label>
              <div className="text-xs text-muted-foreground">用於登入此管理後台。修改後需用新密碼重新登入。</div>
              <Input id="set-admin-key" value={adminKeyInput} onChange={(e) => setAdminKeyInput(e.target.value)} />
            </div>
            <div className="flex flex-col gap-2">
              <Label htmlFor="set-gateway-key">網關 API Key（可選）</Label>
              <div className="text-xs text-muted-foreground">
                設定後，呼叫 <code className="rounded bg-muted px-1">/v1/messages</code> 須攜帶{' '}
                <code className="rounded bg-muted px-1">Authorization: Bearer &lt;key&gt;</code> 或{' '}
                <code className="rounded bg-muted px-1">x-api-key</code>。留空則不校驗。
              </div>
              <Input
                id="set-gateway-key"
                value={gatewayKey}
                onChange={(e) => setGatewayKey(e.target.value)}
                placeholder="留空表示不啟用"
              />
            </div>
            <div className="flex flex-col gap-2">
              <Label htmlFor="set-quota-interval">額度刷新間隔（秒）</Label>
              <div className="text-xs text-muted-foreground">
                後台自動刷新各帳號額度與狀態的週期。設為 0 關閉自動刷新（仍可手動刷新）。修改後即時生效。
              </div>
              <Input
                id="set-quota-interval"
                type="number"
                min={0}
                step={5}
                value={quotaInterval}
                onChange={(e) => setQuotaInterval(e.target.value)}
              />
            </div>
            <div className="flex flex-col gap-2">
              <Label htmlFor="set-rate-limit-threshold">限流降級閾值（連續 429 次數）</Label>
              <div className="text-xs text-muted-foreground">
                帳號連續收到多少次上游 429 後，才標記為「限流」並冷卻一段時間。1 = 一次 429 即降級（預設）；設為更大值可容忍偶發限流；0 = 不降級（429 只切換下一個帳號，不標記冷卻）。修改後即時生效。
              </div>
              <Input
                id="set-rate-limit-threshold"
                type="number"
                min={0}
                step={1}
                value={rateLimitThreshold}
                onChange={(e) => setRateLimitThreshold(e.target.value)}
              />
            </div>
            <div className="flex justify-end">
              <Button type="submit" disabled={saving}>
                {saving ? <Loader2 className="animate-spin" /> : null}
                儲存
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      {/* 使用說明 */}
      <Card>
        <CardContent className="flex flex-col gap-3">
          <div className="text-sm font-semibold">使用說明</div>
          <ul className="list-disc space-y-1.5 pl-5 text-sm leading-relaxed text-muted-foreground marker:text-muted-foreground/60">
            <li>在「帳號池」貼上 Coding Plan JWT 或 API Key 即可加入輪詢。</li>
            <li>請求按 round-robin 分發；某帳號額度用完會自動切到下一個帳號。</li>
            <li>帳號額度、狀態在「帳號池」頁即時刷新展示。</li>
            <li>
              對話端點：<code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">{location.origin}/v1/messages</code>
              （相容 Anthropic Messages 協議）。
            </li>
          </ul>
        </CardContent>
      </Card>
    </div>
  )
}
