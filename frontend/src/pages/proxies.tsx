/* 代理設定頁：線路清單、目前出口測試、新增／編輯／刪除／測試線路 */
import { Activity, Pencil, Plus, Trash2 } from 'lucide-react'
import { useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { useConfirm } from '@/components/confirm'
import { Empty, PanelCard } from '@/components/panel'
import { Button } from '@/components/ui/button'
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
import { api, errMsg } from '@/lib/api'
import { proxyScheme } from '@/lib/format'
import type { EgressInfo, ProxyProfile } from '@/lib/types'

interface ProxiesResponse {
  profiles: ProxyProfile[]
}

/* 目前出口測試狀態 */
type CurrentResult =
  | { state: 'idle' }
  | { state: 'testing' }
  | { state: 'ok'; main: string; sub: string }
  | { state: 'error'; text: string }

/* 單線路測試結果 */
interface RowResult {
  state: 'testing' | 'ok' | 'error'
  text: string
}

function maskUrl(url: string): string {
  return String(url || '').replace(/\/\/([^@/]+)@/, '//***@')
}

function formatProbe(d: EgressInfo): string {
  return [d.ip, d.asn, d.operator, d.latency_ms != null ? d.latency_ms + ' ms' : ''].filter(Boolean).join(' · ')
}

function countryLabel(d: EgressInfo): string {
  return [d.country_code, d.country].filter(Boolean).join(' ')
}

export function ProxiesPage() {
  const qc = useQueryClient()
  const { confirm, element: confirmElement } = useConfirm()

  const { data } = useQuery({
    queryKey: ['proxies'],
    queryFn: () => api<ProxiesResponse>('GET', '/proxies'),
  })
  const profiles = data?.profiles ?? []

  const [current, setCurrent] = useState<CurrentResult>({ state: 'idle' })
  const [testingCurrent, setTestingCurrent] = useState(false)
  const [rowResults, setRowResults] = useState<Record<string, RowResult>>({})
  const testSeqRef = useRef<Record<string, number>>({})

  const [modalOpen, setModalOpen] = useState(false)
  const [editingId, setEditingId] = useState('')
  const [name, setName] = useState('')
  const [url, setUrl] = useState('')
  const [saving, setSaving] = useState(false)

  function invalidate() {
    void qc.invalidateQueries({ queryKey: ['proxies'] })
  }

  function openModal(p?: ProxyProfile) {
    setEditingId(p?.id ?? '')
    setName(p?.name ?? '')
    setUrl(p?.url ?? '')
    setModalOpen(true)
  }

  async function save() {
    if (!url.trim()) {
      toast.error('請輸入代理地址')
      return
    }
    setSaving(true)
    try {
      await api(editingId ? 'PUT' : 'POST', editingId ? '/proxies/' + encodeURIComponent(editingId) : '/proxies', {
        name: name.trim(),
        url: url.trim(),
        enabled: true,
      })
      setModalOpen(false)
      toast.success(editingId ? '代理已更新' : '代理已建立')
      invalidate()
    } catch (e) {
      toast.error('儲存失敗：' + errMsg(e))
    } finally {
      setSaving(false)
    }
  }

  function deleteProxy(p: ProxyProfile) {
    confirm({
      title: '刪除代理',
      danger: true,
      description: (
        <>
          確認刪除 <code className="rounded bg-muted px-1 py-0.5">{p.name}</code>？使用此線路的帳號將切換為直連。
        </>
      ),
      onConfirm: async () => {
        try {
          await api('DELETE', '/proxies/' + encodeURIComponent(p.id))
          toast.success('代理已刪除')
          invalidate()
        } catch (e) {
          toast.error('刪除失敗：' + errMsg(e))
        }
      },
    })
  }

  async function testProxy(p: ProxyProfile) {
    if (rowResults[p.id]?.state === 'testing') return
    const seq = (testSeqRef.current[p.id] ?? 0) + 1
    testSeqRef.current[p.id] = seq
    setRowResults((m) => ({ ...m, [p.id]: { state: 'testing', text: '正在測試線路…' } }))
    try {
      const d = await api<EgressInfo>('POST', '/proxies/' + encodeURIComponent(p.id) + '/test')
      if (testSeqRef.current[p.id] !== seq) return
      setRowResults((m) => ({ ...m, [p.id]: { state: 'ok', text: formatProbe(d) } }))
      toast.success('代理線路正常')
    } catch (e) {
      if (testSeqRef.current[p.id] !== seq) return
      setRowResults((m) => ({ ...m, [p.id]: { state: 'error', text: errMsg(e) } }))
      toast.error('代理測試失敗：' + errMsg(e))
    }
  }

  async function testCurrentLine() {
    setTestingCurrent(true)
    setCurrent({ state: 'testing' })
    try {
      const d = await api<EgressInfo>('POST', '/proxies/test-current')
      setCurrent({
        state: 'ok',
        main: [d.ip, d.asn, d.operator].filter(Boolean).join(' · '),
        sub: [countryLabel(d), d.latency_ms != null ? d.latency_ms + ' ms' : '', d.source ? '來源 ' + d.source : '']
          .filter(Boolean)
          .join(' · '),
      })
      toast.success('目前線路查詢完成')
    } catch (e) {
      setCurrent({ state: 'error', text: errMsg(e) })
      toast.error('目前線路測試失敗：' + errMsg(e))
    } finally {
      setTestingCurrent(false)
    }
  }

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6">
      {/* 頁首 */}
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">代理設定</h1>
          <p className="text-sm text-muted-foreground">集中管理 HTTP／SOCKS5 出口線路</p>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" onClick={() => void testCurrentLine()} disabled={testingCurrent}>
            <Activity /> 測試目前線路
          </Button>
          <Button size="sm" onClick={() => openModal()}>
            <Plus /> 新增代理
          </Button>
        </div>
      </div>

      {/* 伺服器目前出口 */}
      <div
        aria-live="polite"
        className={
          'flex items-center gap-3 rounded-xl border bg-card px-4 py-3 ' +
          (current.state === 'error' ? 'border-destructive/40' : '')
        }
      >
        <span
          className={
            'size-2.5 shrink-0 rounded-full ' +
            (current.state === 'ok'
              ? 'bg-emerald-500'
              : current.state === 'error'
                ? 'bg-red-500'
                : current.state === 'testing'
                  ? 'animate-pulse bg-amber-500'
                  : 'bg-muted-foreground/30')
          }
        />
        <div className="min-w-0 flex-1 leading-tight">
          <span className="block text-xs text-muted-foreground">伺服器目前出口</span>
          <strong className="block truncate text-sm">
            {current.state === 'ok'
              ? current.main
              : current.state === 'testing'
                ? '正在查詢出口…'
                : current.state === 'error'
                  ? '出口查詢失敗'
                  : '尚未測試'}
          </strong>
          <small className="block truncate text-xs text-muted-foreground">
            {current.state === 'ok'
              ? current.sub
              : current.state === 'testing'
                ? '正在連線至 IP 查詢服務'
                : current.state === 'error'
                  ? current.text
                  : '透過 ip.sb 等服務查詢公網 IP 與 ASN'}
          </small>
        </div>
      </div>

      {/* 線路清單＋連線格式說明 */}
      <div className="grid gap-4 lg:grid-cols-5">
        <PanelCard title="代理線路" subtitle="建立後可在帳號設定中選用" badge={`${profiles.length} 條線路`} className="lg:col-span-3">
          {profiles.length ? (
            <div className="flex flex-col divide-y">
              {profiles.map((p) => {
                const result = rowResults[p.id]
                return (
                  <div key={p.id} className="flex flex-wrap items-center gap-3 py-3 first:pt-0 last:pb-0">
                    <span className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-muted text-muted-foreground [&>svg]:size-4">
                      <Activity />
                    </span>
                    <div className="min-w-0 flex-1 leading-tight">
                      <strong className="block truncate text-sm">{p.name}</strong>
                      <small className="block truncate font-mono text-xs text-muted-foreground">{maskUrl(p.url)}</small>
                      {result && (
                        <em
                          className={
                            'mt-0.5 block truncate text-xs not-italic ' +
                            (result.state === 'ok'
                              ? 'text-emerald-600'
                              : result.state === 'error'
                                ? 'text-destructive'
                                : 'text-muted-foreground')
                          }
                        >
                          {result.text}
                        </em>
                      )}
                    </div>
                    <span className="rounded-md bg-muted px-2 py-1 text-xs font-medium">{proxyScheme(p.url)}</span>
                    <span
                      className={
                        'flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs ' +
                        (p.enabled ? 'bg-emerald-100 text-emerald-700' : 'bg-muted text-muted-foreground')
                      }
                    >
                      <span className={'size-1.5 rounded-full ' + (p.enabled ? 'bg-emerald-500' : 'bg-muted-foreground/50')} />
                      {p.enabled ? '啟用' : '停用'}
                    </span>
                    <span className="flex gap-0.5">
                      <Button variant="ghost" size="icon-sm" title="測試線路" onClick={() => void testProxy(p)}>
                        <Activity />
                      </Button>
                      <Button variant="ghost" size="icon-sm" title="編輯" onClick={() => openModal(p)}>
                        <Pencil />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        className="text-destructive hover:text-destructive"
                        title="刪除"
                        onClick={() => deleteProxy(p)}
                      >
                        <Trash2 />
                      </Button>
                    </span>
                  </div>
                )
              })}
            </div>
          ) : (
            <Empty>尚未建立代理線路</Empty>
          )}
        </PanelCard>

        <PanelCard title="連線格式" subtitle="支援帶認證的代理" className="lg:col-span-2">
          <div className="flex flex-col gap-3">
            {(
              [
                ['HTTP', 'http://host:port', 'bg-blue-100 text-blue-700'],
                ['S5', 'socks5://host:port', 'bg-violet-100 text-violet-700'],
                ['AUTH', 'http://user:pass@host:port', 'bg-amber-100 text-amber-700'],
              ] as [string, string, string][]
            ).map(([mark, sample, cls]) => (
              <div key={mark} className="flex items-center gap-3">
                <span className={'w-12 shrink-0 rounded-md px-2 py-1 text-center text-xs font-semibold ' + cls}>{mark}</span>
                <code className="truncate rounded-md bg-muted px-2.5 py-1.5 font-mono text-xs">{sample}</code>
              </div>
            ))}
            <p className="rounded-lg bg-muted/60 px-3 py-2 text-xs text-muted-foreground">
              僅接受 HTTP、HTTPS、SOCKS4、SOCKS5 代理地址，可包含 user:pass 認證。帳號是否使用代理、使用哪條線路，請在「帳號池」的帳號設定中選擇。
            </p>
          </div>
        </PanelCard>
      </div>

      {/* 新增／編輯代理對話框 */}
      <Dialog open={modalOpen} onOpenChange={setModalOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{editingId ? '編輯代理出口' : '新增代理出口'}</DialogTitle>
            <DialogDescription>支援帶認證的 HTTP／SOCKS5 地址。</DialogDescription>
          </DialogHeader>
          <div className="flex flex-col gap-4">
            <div className="flex flex-col gap-2">
              <Label htmlFor="proxy-name">名稱</Label>
              <Input id="proxy-name" placeholder="例如：香港出口 1" value={name} onChange={(e) => setName(e.target.value)} />
            </div>
            <div className="flex flex-col gap-2">
              <Label htmlFor="proxy-url">地址</Label>
              <Input
                id="proxy-url"
                placeholder="http://host:port 或 socks5://host:port"
                autoComplete="off"
                value={url}
                onChange={(e) => setUrl(e.target.value)}
              />
            </div>
            <p className="text-xs text-muted-foreground">僅接受 HTTP、HTTPS、SOCKS4、SOCKS5 代理地址，可包含 user:pass 認證。</p>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setModalOpen(false)}>取消</Button>
            <Button disabled={saving} onClick={() => void save()}>儲存</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {confirmElement}
    </div>
  )
}
