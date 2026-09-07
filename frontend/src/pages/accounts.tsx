/* 帳號池頁：統計卡、篩選、帳號明細表與新增／編輯對話框（輪詢 5 秒） */
import {
  Download,
  Loader2,
  Pencil,
  Plus,
  RefreshCw,
  RotateCcw,
  Trash2,
  Upload,
  Users,
  XCircle,
} from 'lucide-react'
import { useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { useConfirm } from '@/components/confirm'
import { QuotaRows } from '@/components/quota-rows'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Textarea } from '@/components/ui/textarea'
import { api, errMsg } from '@/lib/api'
import { fmt, fmtCompact, fmtDate, normalizeModel, proxyScheme } from '@/lib/format'
import { STATUS_LABEL, type Account, type AccountStatus, type AccountsResponse, type ProxyProfile } from '@/lib/types'

/* 頂層「直連」的 Select 哨兵值：Radix Select 不允許空字串 value */
const PROXY_DIRECT = '__direct__'
const PROXY_LEGACY = '__legacy__'

const STATUS_BADGE: Record<AccountStatus, string> = {
  active: 'bg-emerald-100 text-emerald-700',
  exhausted: 'bg-purple-100 text-purple-700',
  cooling: 'bg-amber-100 text-amber-700',
  invalid: 'bg-red-100 text-red-700',
  disabled: 'bg-muted text-muted-foreground',
}

type FilterKey = 'all' | AccountStatus

export function AccountsPage() {
  const qc = useQueryClient()
  const { confirm, element: confirmElement } = useConfirm()
  const fileRef = useRef<HTMLInputElement>(null)

  const { data } = useQuery({
    queryKey: ['accounts'],
    queryFn: () => api<AccountsResponse>('GET', '/accounts'),
    refetchInterval: 5000,
  })

  const allAccounts = data?.accounts ?? []
  const proxies = data?.proxies ?? []
  const availableModels = data?.models ?? []

  const [filter, setFilter] = useState<FilterKey>('all')
  const [refreshing, setRefreshing] = useState<Set<string>>(new Set())

  /* 新增對話框 */
  const [addOpen, setAddOpen] = useState(false)
  const [addTab, setAddTab] = useState<'login' | 'paste'>('login')
  const [tokensText, setTokensText] = useState('')
  const [addProxy, setAddProxy] = useState(PROXY_DIRECT)
  const [adding, setAdding] = useState(false)
  /* 授權登入流程狀態 */
  const [flow, setFlow] = useState<{ flowId: string; url: string } | null>(null)
  const [callbackUrl, setCallbackUrl] = useState('')
  const [loginStatus, setLoginStatus] = useState('')
  const [loginErr, setLoginErr] = useState(false)
  const [starting, setStarting] = useState(false)
  const [completing, setCompleting] = useState(false)

  /* 編輯對話框 */
  const [editOpen, setEditOpen] = useState(false)
  const [editId, setEditId] = useState('')
  const [editName, setEditName] = useState('')
  const [editToken, setEditToken] = useState('')
  const [editProxy, setEditProxy] = useState(PROXY_DIRECT)
  const [editLegacy, setEditLegacy] = useState(false)
  const [editModels, setEditModels] = useState<string[]>([])
  const [editModelOptions, setEditModelOptions] = useState<[string, string][]>([])
  const [saving, setSaving] = useState(false)

  function invalidate() {
    void qc.invalidateQueries({ queryKey: ['accounts'] })
  }

  /* ── 統計計算（語義照搬舊版 renderStats） ── */
  let totalRem = 0
  let totalQuota = 0
  allAccounts.forEach((a) => {
    Object.values(a.quota || {}).forEach((w) => {
      totalRem += Number(w.remaining) || 0
      totalQuota += Number(w.total) || 0
    })
  })
  const quotaPct = totalQuota > 0 ? Math.max(0, Math.min(100, (totalRem / totalQuota) * 100)) : 0
  const quotaColor = quotaPct <= 15 ? '#ef4444' : quotaPct <= 40 ? '#f59e0b' : '#22c55e'
  const stats = data?.stats

  /* ── 篩選 ── */
  const counts: Record<string, number> = { all: allAccounts.length, exhausted: 0, disabled: 0 }
  allAccounts.forEach((a) => {
    counts[a.status] = (counts[a.status] || 0) + 1
  })
  const filtered = filter === 'all' ? allAccounts : allAccounts.filter((a) => a.status === filter)

  /* ── 新增 ── */
  function openAdd() {
    setTokensText('')
    setAddProxy(PROXY_DIRECT)
    setAddTab('login')
    setFlow(null)
    setCallbackUrl('')
    setLoginStatus('')
    setLoginErr(false)
    setAddOpen(true)
  }

  async function doAdd() {
    const list = tokensText.split('\n').map((s) => s.trim()).filter(Boolean)
    if (!list.length) {
      toast.error('請輸入至少一個 Token')
      return
    }
    setAdding(true)
    try {
      const d = await api<{ count: number }>('POST', '/accounts', {
        tokens: list,
        proxy_id: addProxy === PROXY_DIRECT ? null : addProxy,
      })
      setAddOpen(false)
      toast.success(`新增 ${d.count} 個帳號`)
      invalidate()
    } catch (e) {
      toast.error('新增失敗：' + errMsg(e))
    } finally {
      setAdding(false)
    }
  }

  async function startLogin() {
    setStarting(true)
    try {
      const d = await api<{ flow_id: string; authorize_url: string }>('POST', '/login/start')
      setFlow({ flowId: d.flow_id, url: d.authorize_url })
      setLoginStatus('已產生授權連結，複製到瀏覽器開啟並完成 Z.AI 登入…')
      setLoginErr(false)
    } catch (e) {
      toast.error('發起登入失敗：' + errMsg(e))
    } finally {
      setStarting(false)
    }
  }

  function copyLoginUrl() {
    if (!flow) return
    navigator.clipboard
      .writeText(flow.url)
      .then(() => toast.success('已複製'))
      .catch(() => toast.error('複製失敗'))
  }

  function openLoginUrl() {
    if (flow) window.open(flow.url, '_blank', 'noopener')
  }

  async function completeLogin() {
    if (!flow) {
      toast.error('請先開始登入')
      return
    }
    const url = callbackUrl.trim()
    if (!url) {
      toast.error('請貼上登入完成頁的完整地址')
      return
    }
    setCompleting(true)
    setLoginStatus('正在驗證並匯入帳號…')
    setLoginErr(false)
    try {
      const d = await api<{ status: string; message?: string }>(
        'POST',
        '/login/complete/' + encodeURIComponent(flow.flowId),
        { callback_url: url },
      )
      if (d.status !== 'ready') throw new Error(d.message || '授權結果無效')
      toast.success('登入成功，已匯入帳號池')
      setAddOpen(false)
      invalidate()
    } catch (e) {
      setLoginStatus(errMsg(e) || '完成授權失敗')
      setLoginErr(true)
    } finally {
      setCompleting(false)
    }
  }

  /* ── 編輯 ── */
  function openEdit(a: Account) {
    setEditId(a.id)
    setEditName(a.name || '')
    setEditToken('')
    setEditLegacy(Boolean(a.proxy_url && !a.proxy_id))
    setEditProxy(a.proxy_id || (a.proxy_url && !a.proxy_id ? PROXY_LEGACY : PROXY_DIRECT))
    /* 可設定模型：全域模型清單＋該帳號額度模型＋已停用模型，正規化去重 */
    const models = new Map<string, string>()
    const quotaModels = Object.entries(a.quota || {}).map(([k, w]) => w.model || k)
    ;[...availableModels, ...quotaModels, ...(a.disabled_models || [])].forEach((model) => {
      const key = normalizeModel(model)
      if (key && !models.has(key)) models.set(key, String(model))
    })
    setEditModelOptions([...models])
    const disabled = new Set((a.disabled_models || []).map(normalizeModel))
    setEditModels([...disabled])
    setEditOpen(true)
  }

  async function doEdit() {
    setSaving(true)
    try {
      const payload: Record<string, unknown> = {
        name: editName.trim(),
        disabled_models: editModels,
        ...(editToken.trim() && { token: editToken.trim() }),
      }
      if (editProxy !== PROXY_LEGACY) payload.proxy_id = editProxy === PROXY_DIRECT ? null : editProxy
      await api('PUT', '/accounts/' + editId, payload)
      setEditOpen(false)
      toast.success('已儲存')
      invalidate()
    } catch (e) {
      toast.error('儲存失敗：' + errMsg(e))
    } finally {
      setSaving(false)
    }
  }

  /* ── 列操作 ── */
  function doDelete(a: Account) {
    const label = a.email || a.name || a.id
    confirm({
      title: '刪除帳號',
      danger: true,
      description: (
        <>
          確認刪除 <code className="rounded bg-muted px-1 py-0.5">{label}</code>？此操作無法復原。
        </>
      ),
      onConfirm: async () => {
        try {
          await api('DELETE', '/accounts', [a.id])
          toast.success('已刪除')
          invalidate()
        } catch (e) {
          toast.error('刪除失敗：' + errMsg(e))
        }
      },
    })
  }

  async function toggleEnabled(a: Account) {
    try {
      await api('POST', '/accounts/' + a.id + '/enabled', { enabled: a.status === 'disabled' })
      invalidate()
    } catch (e) {
      toast.error('操作失敗：' + errMsg(e))
    }
  }

  async function refreshOne(a: Account) {
    if (refreshing.has(a.id)) return
    setRefreshing((s) => new Set(s).add(a.id))
    try {
      await api('POST', '/accounts/' + a.id + '/refresh')
      toast.success('額度已重新整理')
    } catch (e) {
      toast.error('重新整理失敗：' + errMsg(e))
    } finally {
      setRefreshing((s) => {
        const next = new Set(s)
        next.delete(a.id)
        return next
      })
      invalidate()
    }
  }

  async function refreshAll() {
    toast.info('正在重新整理全部額度…')
    try {
      const d = await api<{ summary: { ok: number; fail: number } }>('POST', '/accounts/refresh', { all: true })
      toast.success(`重新整理完成：成功 ${d.summary.ok}，失敗 ${d.summary.fail}`)
      invalidate()
    } catch (e) {
      toast.error('重新整理失敗：' + errMsg(e))
    }
  }

  function resetStats(a: Account) {
    const label = a.email || a.name || a.id
    confirm({
      title: '重置 Token 統計',
      danger: true,
      description: (
        <>
          確認清零 <code className="rounded bg-muted px-1 py-0.5">{label}</code> 的累計 Token 用量？此操作不可撤銷。
        </>
      ),
      onConfirm: async () => {
        try {
          await api('POST', '/accounts/' + a.id + '/reset-stats')
          toast.success('已重置統計')
          invalidate()
        } catch (e) {
          toast.error('重置失敗：' + errMsg(e))
        }
      },
    })
  }

  /* ── 匯入／匯出 ── */
  async function doExport() {
    try {
      const d = await api<unknown>('GET', '/export')
      const a = document.createElement('a')
      a.href = URL.createObjectURL(new Blob([JSON.stringify(d, null, 2)], { type: 'application/json' }))
      a.download = 'zcode-accounts.json'
      a.click()
      URL.revokeObjectURL(a.href)
    } catch (e) {
      toast.error('匯出失敗：' + errMsg(e))
    }
  }

  async function onImportFile(ev: React.ChangeEvent<HTMLInputElement>) {
    const file = ev.target.files?.[0]
    if (!file) return
    try {
      const payload: unknown = JSON.parse(await file.text())
      const d = await api<{ count: number }>('POST', '/import', payload)
      toast.success(`匯入 ${d.count} 個帳號`)
      invalidate()
    } catch (e) {
      toast.error('匯入失敗：' + errMsg(e))
    }
    ev.target.value = ''
  }

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6">
      {/* 頁首 */}
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">帳號池</h1>
          <p className="text-sm text-muted-foreground">多帳號輪詢 · 額度用完自動切換下一個帳號 · 即時用量監控</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="mr-1 flex items-center gap-1.5 text-xs text-muted-foreground">
            <span className="size-1.5 animate-pulse rounded-full bg-emerald-500" />
            即時監控中
          </span>
          <Button variant="outline" size="sm" onClick={() => fileRef.current?.click()}>
            <Upload data-slot="icon" /> 匯入
          </Button>
          <Button variant="outline" size="sm" onClick={() => void doExport()}>
            <Download /> 匯出
          </Button>
          <Button variant="outline" size="sm" onClick={() => void refreshAll()}>
            <RefreshCw /> 重新整理額度
          </Button>
          <Button size="sm" onClick={openAdd}>
            <Plus /> 新增
          </Button>
        </div>
      </div>
      <input ref={fileRef} type="file" accept=".json" hidden onChange={(e) => void onImportFile(e)} />

      {/* 帳號概覽 */}
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-5">
        <StatCell label="帳號總數" value={fmt(stats?.total)} icon={<Users className="size-4" />} />
        <StatCell label="正常" value={fmt(stats?.active)} color="#16a34a" icon={<span className="size-2 rounded-full bg-emerald-500" />} />
        <StatCell label="額度用完" value={fmt(stats?.exhausted)} color="#8d6bbd" icon={<span className="size-2 rounded-full bg-purple-500" />} />
        <Card>
          <CardContent className="flex flex-col gap-1">
            <div className="flex items-center justify-between text-xs text-muted-foreground">
              總額度（剩餘）
              <span className="size-2 rounded-full" style={{ background: quotaColor }} />
            </div>
            <div className="text-2xl font-semibold tabular-nums" style={{ color: '#4c9168' }}>
              {fmtCompact(totalRem)}
            </div>
            <div className="h-1.5 overflow-hidden rounded-full bg-muted" role="progressbar" aria-valuenow={quotaPct} aria-valuemin={0} aria-valuemax={100}>
              <span className="block h-full rounded-full" style={{ width: `${quotaPct}%`, background: quotaColor }} />
            </div>
            <div className="text-[11px] text-muted-foreground">
              {totalQuota ? `${fmtCompact(totalRem)} / ${fmtCompact(totalQuota)} · ${quotaPct.toFixed(1)}%` : '尚無額度資料'}
            </div>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="flex flex-col gap-1" title={`輸入 ${fmt(stats?.tokens_in)} · 輸出 ${fmt(stats?.tokens_out)} · 快取 ${fmt(stats?.tokens_cache)}`}>
            <div className="flex items-center justify-between text-xs text-muted-foreground">
              累計調度 Tokens（入+出）
              <span className="size-2 rounded-full bg-blue-500" />
            </div>
            <div className="text-2xl font-semibold tabular-nums" style={{ color: '#4c76b2' }}>
              {fmtCompact(Number(stats?.tokens_in) + Number(stats?.tokens_out))}
            </div>
            <div className="text-[11px] text-muted-foreground">入 {fmtCompact(stats?.tokens_in)} · 出 {fmtCompact(stats?.tokens_out)}</div>
          </CardContent>
        </Card>
      </div>

      {/* 明細標題＋篩選 */}
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2 text-sm font-medium">
          帳號明細 <Badge variant="secondary">{filtered.length}</Badge>
        </div>
        <span className="text-xs text-muted-foreground">
          {data ? '更新於 ' + new Date(data.ts * 1000).toLocaleTimeString('zh-TW') : ''}
        </span>
      </div>
      <div className="flex flex-wrap gap-2">
        {(
          [
            ['all', '全部'],
            ['exhausted', '用完'],
            ['disabled', '停用'],
          ] as [FilterKey, string][]
        ).map(([k, l]) => (
          <Button
            key={k}
            size="sm"
            variant={filter === k ? 'default' : 'outline'}
            className="h-8 rounded-full"
            onClick={() => setFilter(k)}
          >
            {l}
            <span className="tabular-nums opacity-70">{counts[k] || 0}</span>
          </Button>
        ))}
      </div>

      {/* 帳號明細表 */}
      <Card>
        <CardContent className="overflow-x-auto px-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>賬號</TableHead>
                <TableHead className="w-20 text-center">狀態</TableHead>
                <TableHead className="w-36">出口線路</TableHead>
                <TableHead className="min-w-56">額度</TableHead>
                <TableHead className="w-16 text-center">呼叫</TableHead>
                <TableHead className="w-16 text-center">失敗</TableHead>
                <TableHead className="w-24 text-center">Tokens</TableHead>
                <TableHead className="w-28">最近使用</TableHead>
                <TableHead className="w-40 text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {!filtered.length ? (
                <TableRow>
                  <TableCell colSpan={9} className="py-10 text-center text-sm text-muted-foreground">
                    尚無帳號，請點擊右上角「新增」
                  </TableCell>
                </TableRow>
              ) : (
                filtered.map((a) => (
                  <TableRow key={a.id}>
                    <TableCell>
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="font-medium">{a.email || a.name || '未命名帳號'}</span>
                        {(a.disabled_models || []).length > 0 && (
                          <Badge variant="outline" className="text-[11px] font-normal" title={(a.disabled_models || []).join('、')}>
                            停用 {a.disabled_models.length} 模型
                          </Badge>
                        )}
                      </div>
                    </TableCell>
                    <TableCell className="text-center">
                      <Badge className={STATUS_BADGE[a.status]}>{STATUS_LABEL[a.status] || a.status}</Badge>
                    </TableCell>
                    <TableCell>
                      {!a.proxy_url ? (
                        <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
                          <span className="size-1.5 rounded-full bg-muted-foreground/40" />
                          直連
                        </span>
                      ) : (
                        <span className="flex items-center gap-1.5 text-xs">
                          <span className="size-1.5 rounded-full bg-emerald-500/70" />
                          {proxies.find((p) => p.id === a.proxy_id)?.name || '舊版自訂代理'}
                        </span>
                      )}
                    </TableCell>
                    <TableCell className="min-w-56">
                      <QuotaRows account={a} />
                    </TableCell>
                    <TableCell className="text-center tabular-nums text-muted-foreground">{a.use_count || 0}</TableCell>
                    <TableCell className="text-center tabular-nums text-muted-foreground">{a.fail_count || 0}</TableCell>
                    <TableCell>
                      <TokensCell account={a} />
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">{fmtDate(a.last_used_at)}</TableCell>
                    <TableCell>
                      <div className="flex justify-end gap-0.5">
                        {a.mode === 'jwt' && (
                          <Button variant="ghost" size="icon-sm" title="重新整理額度" onClick={() => void refreshOne(a)}>
                            {refreshing.has(a.id) ? <Loader2 className="animate-spin" /> : <RefreshCw />}
                          </Button>
                        )}
                        <Button variant="ghost" size="icon-sm" title="重置 Token 統計" onClick={() => resetStats(a)}>
                          <RotateCcw />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          title={a.status === 'disabled' ? '恢復' : '停用'}
                          onClick={() => void toggleEnabled(a)}
                        >
                          {a.status === 'disabled' ? <RotateCcw /> : <XCircle />}
                        </Button>
                        <Button variant="ghost" size="icon-sm" title="編輯" onClick={() => openEdit(a)}>
                          <Pencil />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          className="text-destructive hover:text-destructive"
                          title="刪除"
                          onClick={() => doDelete(a)}
                        >
                          <Trash2 />
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      {/* 新增帳號對話框：授權登入為預設分頁 */}
      <Dialog open={addOpen} onOpenChange={setAddOpen}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>新增帳號</DialogTitle>
            <DialogDescription>支援授權登入或貼上憑證，貼上時每行一個。</DialogDescription>
          </DialogHeader>
          <Tabs value={addTab} onValueChange={(v) => setAddTab(v as 'login' | 'paste')}>
            <TabsList className="w-full">
              <TabsTrigger value="login" className="flex-1">授權登入</TabsTrigger>
              <TabsTrigger value="paste" className="flex-1">貼上憑證</TabsTrigger>
            </TabsList>

            {/* 授權登入 */}
            <TabsContent value="login" className="flex flex-col gap-3">
              <p className="text-xs text-muted-foreground">
                Z.AI 只允許 ZCode 官方回呼地址，需要在登入完成後複製一次瀏覽器地址，不需要提供帳號密碼。
              </p>
              {!flow ? (
                <Button className="h-10 w-full" disabled={starting} onClick={() => void startLogin()}>
                  {starting ? <Loader2 className="animate-spin" /> : null}
                  開始登入
                </Button>
              ) : (
                <div className="flex flex-col gap-3">
                  <p className="text-xs text-muted-foreground">1. 開啟授權頁並完成 Z.AI 登入：</p>
                  <div className="flex items-center gap-2">
                    <Input readOnly value={flow.url} className="min-w-0 flex-1 font-mono text-xs" />
                    <Button variant="outline" size="sm" onClick={copyLoginUrl}>複製</Button>
                    <Button size="sm" onClick={openLoginUrl}>開啟</Button>
                  </div>
                  <p className="text-xs text-muted-foreground">2. 頁面顯示「登入已完成」後，複製該頁面瀏覽器網址列的完整地址並貼到這裡：</p>
                  <Textarea
                    rows={3}
                    className="font-mono text-xs"
                    placeholder="https://zcode.z.ai/app/oauth/login?... 或 zcode://oauth/callback?..."
                    value={callbackUrl}
                    onChange={(e) => setCallbackUrl(e.target.value)}
                  />
                  <Button className="h-10 w-full" disabled={completing} onClick={() => void completeLogin()}>
                    {completing ? <Loader2 className="animate-spin" /> : null}
                    3. 完成匯入
                  </Button>
                  <p className={'text-xs ' + (loginErr ? 'text-destructive' : 'text-muted-foreground')}>{loginStatus}</p>
                </div>
              )}
            </TabsContent>

            {/* 貼上憑證 */}
            <TabsContent value="paste" className="flex flex-col gap-4">
              <div className="flex flex-col gap-2">
                <p className="text-xs text-muted-foreground">每行一個 Coding Plan JWT 或 API Key，已存在的會自動跳過</p>
                <Textarea
                  rows={7}
                  className="font-mono text-xs"
                  placeholder="貼上 JWT / API Key，每行一個..."
                  value={tokensText}
                  onChange={(e) => setTokensText(e.target.value)}
                />
              </div>
              <div className="flex flex-col gap-2">
                <Label>出口線路</Label>
                <ProxySelect value={addProxy} onChange={setAddProxy} proxies={proxies} />
              </div>
            </TabsContent>
          </Tabs>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAddOpen(false)}>取消</Button>
            {addTab === 'paste' && (
              <Button disabled={adding} onClick={() => void doAdd()}>
                {adding ? <Loader2 className="animate-spin" /> : null}
                新增
              </Button>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* 編輯帳號對話框 */}
      <Dialog open={editOpen} onOpenChange={setEditOpen}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>編輯帳號</DialogTitle>
            <DialogDescription>修改名稱、憑證、出口線路或停用特定模型。</DialogDescription>
          </DialogHeader>
          <div className="flex flex-col gap-4">
            <div className="flex flex-col gap-2">
              <Label htmlFor="edit-name">名稱</Label>
              <Input id="edit-name" value={editName} onChange={(e) => setEditName(e.target.value)} />
            </div>
            <div className="flex flex-col gap-2">
              <Label htmlFor="edit-token">Token</Label>
              <Input
                id="edit-token"
                placeholder="留空則不修改"
                value={editToken}
                onChange={(e) => setEditToken(e.target.value)}
              />
            </div>
            <div className="flex flex-col gap-2">
              <Label>出口線路</Label>
              <ProxySelect value={editProxy} onChange={setEditProxy} proxies={proxies} legacy={editLegacy} />
            </div>
            <div className="flex flex-col gap-2">
              <Label>停用模型</Label>
              {editModelOptions.length ? (
                <div className="grid grid-cols-2 gap-2 rounded-lg border p-3">
                  {editModelOptions.map(([key, label]) => (
                    <label key={key} className="flex cursor-pointer items-center gap-2 text-xs">
                      <Checkbox
                        checked={editModels.includes(key)}
                        onCheckedChange={(v) =>
                          setEditModels((list) => (v ? [...list, key] : list.filter((m) => m !== key)))
                        }
                      />
                      {label}
                    </label>
                  ))}
                </div>
              ) : (
                <p className="text-xs text-muted-foreground">尚無可設定模型</p>
              )}
              <p className="text-xs text-muted-foreground">勾選後，此帳號不會參與該模型的任何調度。</p>
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditOpen(false)}>取消</Button>
            <Button disabled={saving} onClick={() => void doEdit()}>
              {saving ? <Loader2 className="animate-spin" /> : null}
              儲存
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {confirmElement}
    </div>
  )
}

/* 統計小卡 */
function StatCell({ label, value, color, icon }: { label: string; value: string; color?: string; icon?: React.ReactNode }) {
  return (
    <Card>
      <CardContent className="flex flex-col gap-1">
        <div className="flex items-center justify-between text-xs text-muted-foreground">
          {label}
          {icon}
        </div>
        <div className="text-2xl font-semibold tabular-nums" style={color ? { color } : undefined}>
          {value}
        </div>
      </CardContent>
    </Card>
  )
}

/* 出口線路下拉：直連／（編輯時）舊版自訂代理／線路清單 */
function ProxySelect({
  value,
  onChange,
  proxies,
  legacy = false,
}: {
  value: string
  onChange: (v: string) => void
  proxies: ProxyProfile[]
  legacy?: boolean
}) {
  return (
    <Select value={value} onValueChange={onChange}>
      <SelectTrigger className="w-full">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value={PROXY_DIRECT}>直連（不使用代理）</SelectItem>
        {legacy && <SelectItem value={PROXY_LEGACY}>舊版自訂代理（保持不變）</SelectItem>}
        {proxies.map((p) => (
          <SelectItem key={p.id} value={p.id}>
            {p.name} · {proxyScheme(p.url)}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

/* 累計 Tokens 欄：入／出／快取三行 */
function TokensCell({ account }: { account: Account }) {
  const t = account.total_tokens || { input: 0, output: 0, cache_creation: 0, cache_read: 0 }
  const i = Number(t.input) || 0
  const o = Number(t.output) || 0
  const c = (Number(t.cache_creation) || 0) + (Number(t.cache_read) || 0)
  if (!i && !o && !c) return <span className="text-muted-foreground">—</span>
  return (
    <div
      className="flex flex-col items-center gap-0.5 text-[11px] leading-tight"
      title={`輸入 ${i.toLocaleString()} · 輸出 ${o.toLocaleString()} · 快取 ${c.toLocaleString()}`}
    >
      <span><span className="text-muted-foreground">入</span> <b className="tabular-nums">{fmtCompact(i)}</b></span>
      <span><span className="text-muted-foreground">出</span> <b className="tabular-nums">{fmtCompact(o)}</b></span>
      {c ? <span><span className="text-muted-foreground">緩</span> <b className="tabular-nums">{fmtCompact(c)}</b></span> : null}
    </div>
  )
}
