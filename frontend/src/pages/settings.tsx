/* 系統設定頁：分頁籤 + 多欄位佈局。
 *
 * 分成「鑑權」「訪客提交」「使用說明」三個分頁：三組設定彼此獨立，混在單欄
 * 長表單裡要捲很久才能找到目標欄位，而它們的修改時機也完全不同（鑑權是初始
 * 配置，訪客提交是對外開放與否的開關）。
 *
 * 欄位以兩欄網格排列（窄螢幕自動堆疊）：標籤、說明、輸入框為一組，說明放在
 * 標籤下方而非輸入框下方，因為它解釋的是「這個欄位是什麼」而不是輸入格式。 */
import { useEffect, useState, type FormEvent, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { KeyRound, Loader2, UserCheck } from 'lucide-react'
import { toast } from 'sonner'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
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
  const [inviteCode, setInviteCode] = useState('')
  const [capInstance, setCapInstance] = useState('')
  const [capSiteKey, setCapSiteKey] = useState('')
  const [capSecret, setCapSecret] = useState('')
  const [showKeys, setShowKeys] = useState(false)
  const [saving, setSaving] = useState(false)

  /* 載入完成後填入表單（僅在尚未編輯時同步） */
  useEffect(() => {
    if (!data) return
    setAdminKeyInput(data.admin_key || '')
    setGatewayKey(data.gateway_key || '')
    setQuotaInterval(String(data.quota_refresh_interval ?? 60))
    setInviteCode(data.guest_invite_code || '')
    setCapInstance(data.cap_instance || '')
    setCapSiteKey(data.cap_site_key || '')
    setCapSecret(data.cap_secret || '')
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
    setSaving(true)
    try {
      await api('PUT', '/settings', {
        admin_key: adminKeyInput.trim(),
        gateway_key: gatewayKey.trim(),
        quota_refresh_interval: interval,
        guest_invite_code: inviteCode.trim(),
        cap_instance: capInstance.trim(),
        cap_site_key: capSiteKey.trim(),
        cap_secret: capSecret.trim(),
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

  /* 人機驗證三項的填寫狀態，決定下方提示的內容 */
  const capFilled = [capInstance, capSiteKey, capSecret].filter((v) => v.trim()).length
  const capEndpoint = capInstance.trim() && capSiteKey.trim()
    ? `${capInstance.trim().replace(/\/+$/, '')}/${capSiteKey.trim().replace(/^\/+|\/+$/g, '')}/siteverify`
    : ''

  return (
    <div className="mx-auto flex w-full max-w-4xl flex-col gap-6">
      {/* 頁首 */}
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">系統設定</h1>
          <p className="text-sm text-muted-foreground">後台鑑權、網關存取控制與訪客提交</p>
        </div>
        <Button type="submit" form="settings-form" disabled={saving}>
          {saving ? <Loader2 className="animate-spin" /> : null}
          儲存設定
        </Button>
      </div>

      <form id="settings-form" onSubmit={save}>
        <Tabs defaultValue="auth" className="gap-5">
          <TabsList className="w-full">
            <TabsTrigger value="auth" className="flex-1">
              <KeyRound className="size-3.5" />
              鑑權
            </TabsTrigger>
            <TabsTrigger value="guest" className="flex-1">
              <UserCheck className="size-3.5" />
              訪客提交
            </TabsTrigger>
            <TabsTrigger value="help" className="flex-1">
              使用說明
            </TabsTrigger>
          </TabsList>

          {/* ── 鑑權 ── */}
          <TabsContent value="auth">
            <Card>
              <CardContent className="flex flex-col gap-5">
                <SectionHead title="鑑權" desc="登入後台與呼叫網關所用的密鑰" />
                <FieldGrid>
                  <Field
                    id="set-admin-key"
                    label="後台密碼"
                    hint="用於登入此管理後台。修改後需用新密碼重新登入。"
                  >
                    <Input
                      id="set-admin-key"
                      type={showKeys ? 'text' : 'password'}
                      value={adminKeyInput}
                      onChange={(e) => setAdminKeyInput(e.target.value)}
                    />
                  </Field>
                  <Field
                    id="set-gateway-key"
                    label="網關 API Key"
                    hint={
                      <>
                        一律必填（fail-closed）：呼叫{' '}
                        <Code>/v1/messages</Code>、<Code>/async/v1/*</Code>、
                        <Code>/v1/models</Code> 須攜帶 <Code>Authorization: Bearer &lt;key&gt;</Code>{' '}
                        或 <Code>x-api-key</Code>。留空儲存會被拒絕。
                      </>
                    }
                  >
                    <Input
                      id="set-gateway-key"
                      type={showKeys ? 'text' : 'password'}
                      value={gatewayKey}
                      onChange={(e) => setGatewayKey(e.target.value)}
                    />
                  </Field>
                </FieldGrid>
                <Field
                  id="set-quota-interval"
                  label="額度刷新間隔（秒）"
                  hint="後台自動刷新各帳號額度與狀態的週期。設為 0 關閉自動刷新（仍可手動刷新）。修改後即時生效。"
                >
                  <Input
                    id="set-quota-interval"
                    type="number"
                    min={0}
                    step={5}
                    value={quotaInterval}
                    onChange={(e) => setQuotaInterval(e.target.value)}
                  />
                </Field>
                <label className="flex w-fit cursor-pointer items-center gap-2 text-xs text-muted-foreground">
                  <Checkbox checked={showKeys} onCheckedChange={(v) => setShowKeys(v === true)} />
                  顯示密鑰明文
                </label>
              </CardContent>
            </Card>
          </TabsContent>

          {/* ── 訪客提交 ── */}
          <TabsContent value="guest" className="flex flex-col gap-5">
            <Card>
              <CardContent className="flex flex-col gap-5">
                <SectionHead
                  title="訪客提交"
                  desc="開放 /guest 頁面，讓訪客透過 Z.AI 授權提交自己的帳號"
                />
                <Field
                  id="set-invite-code"
                  label="訪客邀請碼"
                  hint={
                    <>
                      設定後 <Code>/guest</Code> 頁面即對外開放。
                      <span className="font-medium text-foreground">留空即關閉訪客入口</span>
                      ，每 IP 每日最多提交 3 次。
                    </>
                  }
                >
                  <Input
                    id="set-invite-code"
                    type={showKeys ? 'text' : 'password'}
                    value={inviteCode}
                    placeholder="留空則關閉訪客提交"
                    onChange={(e) => setInviteCode(e.target.value)}
                  />
                </Field>
                <p className="text-xs text-muted-foreground">
                  訪客提交的帳號須通過一次真實請求實測才會入池；失敗的直接丟棄，不回顯任何帳號資訊。
                </p>
              </CardContent>
            </Card>

            <Card>
              <CardContent className="flex flex-col gap-5">
                <SectionHead
                  title="人機驗證（Cap）"
                  desc="自建 Cap 實例的 PoW 驗證，擋住自動化刷取；未配置時整段跳過"
                />
                <FieldGrid>
                  <Field
                    id="set-cap-instance"
                    label="實例地址"
                    hint={
                      <>
                        不含 site key，例如 <Code>https://cap.example.com</Code>。
                        須為訪客瀏覽器可達的地址。
                      </>
                    }
                  >
                    <Input
                      id="set-cap-instance"
                      type="text"
                      value={capInstance}
                      placeholder="https://cap.example.com"
                      onChange={(e) => setCapInstance(e.target.value)}
                    />
                  </Field>
                  <Field
                    id="set-cap-site-key"
                    label="Site Key"
                    hint={
                      <>
                        Cap 後台建立 site key 後取得的識別碼，例如{' '}
                        <Code>d9256640cb53</Code>。
                      </>
                    }
                  >
                    <Input
                      id="set-cap-site-key"
                      type="text"
                      value={capSiteKey}
                      placeholder="d9256640cb53"
                      onChange={(e) => setCapSiteKey(e.target.value)}
                    />
                  </Field>
                </FieldGrid>
                <Field
                  id="set-cap-secret"
                  label="密鑰"
                  hint={
                    <>
                      Cap 後台的 secret key（<span className="font-medium text-foreground">不是</span>
                      管理員 ADMIN_KEY）。只留在服務端，不會下發給瀏覽器。
                    </>
                  }
                >
                  <Input
                    id="set-cap-secret"
                    type={showKeys ? 'text' : 'password'}
                    value={capSecret}
                    onChange={(e) => setCapSecret(e.target.value)}
                  />
                </Field>

                <div
                  className={
                    'rounded-lg px-3 py-2 text-xs ' +
                    (capFilled === 0 || capFilled === 3
                      ? 'bg-muted/50 text-muted-foreground'
                      : 'bg-destructive/10 text-destructive')
                  }
                >
                  {capFilled === 3 ? (
                    <>
                      已啟用，實際呼叫地址：
                      <code className="ml-1 break-all rounded bg-muted px-1 font-mono">
                        {capEndpoint}
                      </code>
                    </>
                  ) : capFilled === 0 ? (
                    '三項皆留空即停用人機驗證。'
                  ) : (
                    `三項須全部填寫才會啟用（目前填了 ${capFilled}/3）；只填部分無法儲存。`
                  )}
                </div>
                <label className="flex w-fit cursor-pointer items-center gap-2 text-xs text-muted-foreground">
                  <Checkbox checked={showKeys} onCheckedChange={(v) => setShowKeys(v === true)} />
                  顯示密鑰與邀請碼明文
                </label>
              </CardContent>
            </Card>
          </TabsContent>

          {/* ── 使用說明 ── */}
          <TabsContent value="help">
            <Card>
              <CardContent className="flex flex-col gap-3">
                <SectionHead title="使用說明" desc="常用操作與端點" />
                <ul className="list-disc space-y-1.5 pl-5 text-sm leading-relaxed text-muted-foreground marker:text-muted-foreground/60">
                  <li>在「帳號池」貼上 Coding Plan JWT 或 API Key 即可加入輪詢。</li>
                  <li>請求按 round-robin 分發；某帳號額度用完會自動切到下一個帳號。</li>
                  <li>帳號額度、狀態在「帳號池」頁即時刷新展示。</li>
                  <li>
                    對話端點：<Code>{location.origin}/v1/messages</Code>（相容 Anthropic Messages 協議）。
                  </li>
                </ul>
              </CardContent>
            </Card>
          </TabsContent>
        </Tabs>
      </form>
    </div>
  )
}

/* ── 佈局小元件 ─────────────────────────────────────────────────────────── */

/* SectionHead 區段標題：標題 + 一句說明，讓每張卡片自解釋 */
function SectionHead({ title, desc }: { title: string; desc: string }) {
  return (
    <div className="flex flex-col gap-0.5">
      <div className="text-sm font-semibold">{title}</div>
      <div className="text-xs text-muted-foreground">{desc}</div>
    </div>
  )
}

/* FieldGrid 兩欄網格：窄螢幕自動堆疊為單欄 */
function FieldGrid({ children }: { children: ReactNode }) {
  return <div className="grid gap-5 sm:grid-cols-2">{children}</div>
}

/* Field 單一欄位：標籤、說明、輸入框為一組。
   說明置於標籤下方而非輸入框下方——它解釋的是欄位用途，不是輸入格式。 */
function Field({
  id,
  label,
  hint,
  children,
}: {
  id: string
  label: string
  hint: ReactNode
  children: ReactNode
}) {
  return (
    <div className="flex flex-col gap-2">
      <Label htmlFor={id}>{label}</Label>
      <div className="text-xs leading-relaxed text-muted-foreground">{hint}</div>
      {children}
    </div>
  )
}

/* Code 行內程式碼片段，統一後台各處的呈現 */
function Code({ children }: { children: ReactNode }) {
  return <code className="rounded bg-muted px-1 font-mono text-[11px]">{children}</code>
}
