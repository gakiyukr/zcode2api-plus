/* 儀表板頁：網關指標卡、提供商概況、帳號健康、Token 組成、帳號調度分布、用量排行、最近活動、網關資訊（輪詢 10 秒） */
import {
  Boxes,
  CircleCheck,
  Database,
  RefreshCw,
  TrendingUp,
  Users,
  Zap,
} from 'lucide-react'
import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { Empty, MetricCard, PanelCard } from '@/components/panel'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { api } from '@/lib/api'
import { fmt, fmtCompact, relativeTime } from '@/lib/format'
import {
  STATUS_COLOR,
  STATUS_LABEL_LONG,
  type Account,
  type AccountsResponse,
  type StatusResponse,
  type UsageResponse,
} from '@/lib/types'

export function DashboardPage() {
  const { data, isFetching, refetch } = useQuery({
    queryKey: ['dashboard'],
    queryFn: async () => {
      const [accountsData, statusData, usageData] = await Promise.all([
        api<AccountsResponse>('GET', '/accounts'),
        api<StatusResponse>('GET', '/status'),
        api<UsageResponse>('GET', '/usage'),
      ])
      return { accountsData, statusData, usageData }
    },
    refetchInterval: 10000,
  })

  const accounts = data?.accountsData.accounts ?? []
  const stats = data?.accountsData.stats
  const providers = data?.accountsData.providers ?? []
  const status = data?.statusData
  const usage = data?.usageData
  const usageCalls = Number(usage?.summary?.calls) || 0

  const calls = Number(stats?.calls) || 0
  const failed = Number(stats?.fail) || 0
  const input = Number(stats?.tokens_in) || 0
  const output = Number(stats?.tokens_out) || 0
  const cache = Number(stats?.tokens_cache) || 0
  const tokens = input + output + cache

  /* 額度彙總 */
  let remaining = 0
  let items = 0
  accounts.forEach((a) =>
    Object.values(a.quota || {}).forEach((q) => {
      remaining += Number(q.remaining) || 0
      items++
    }),
  )
  const pool = Object.values(status?.quota_pool || {}).reduce((n, v) => n + (Number(v) || 0), 0)
  const successRate = calls ? `${Math.max(0, ((calls - failed) / calls) * 100).toFixed(1)}%` : '--'

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6">
      {/* 頁首 */}
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">儀表板</h1>
          <p className="text-sm text-muted-foreground">帳號池與網關的即時運行概況</p>
        </div>
        <div className="flex items-center gap-2">
          <span className="mr-1 flex items-center gap-1.5 text-xs text-muted-foreground">
            <span className="size-1.5 animate-pulse rounded-full bg-emerald-500" />
            即時資料
          </span>
          <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching}>
            <RefreshCw className={isFetching ? 'animate-spin' : undefined} /> 重新整理
          </Button>
        </div>
      </div>

      {/* 網關指標卡 */}
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-3 xl:grid-cols-6">
        <MetricCard icon={<Users />} tone="text-blue-600" label="帳號總數" value={fmt(stats?.total)} detail={`${fmt(stats?.active)} 個正常`} />
        <MetricCard icon={<Boxes />} tone="text-emerald-600" label="可用帳號池" value={fmt(pool)} detail={`${providers.length} 個提供商`} />
        <MetricCard icon={<Zap />} tone="text-violet-600" label="累計呼叫" value={fmt(calls)} detail={`${fmt(failed)} 次失敗`} />
        <MetricCard icon={<CircleCheck />} tone="text-amber-600" label="請求成功率" value={successRate} detail="按累計呼叫計算" />
        <MetricCard icon={<Database />} tone="text-cyan-600" label="累計 Token" value={fmtCompact(tokens)} detail={`輸入 ${fmtCompact(input)} · 輸出 ${fmtCompact(output)}`} />
        <MetricCard icon={<TrendingUp />} tone="text-rose-600" label="剩餘額度" value={fmtCompact(remaining)} detail={`${items} 個額度項目`} />
      </div>

      {/* 提供商概況＋帳號健康 */}
      <div className="grid gap-4 lg:grid-cols-5">
        <PanelCard title="提供商概況" subtitle="帳號、請求與 Token 分布" badge={`${providers.length} 個提供商`} className="lg:col-span-3">
          {providers.length ? (
            <div className="flex flex-col divide-y">
              {providers.map((provider) => {
                const items2 = accounts.filter((a) => a.provider === provider)
                const callsP = items2.reduce((n, a) => n + (Number(a.use_count) || 0), 0)
                const tokensP = items2.reduce((n, a) => n + accountTokens(a), 0)
                const available = items2.filter((a) => a.status === 'active').length
                return (
                  <div key={provider} className="flex flex-wrap items-center gap-x-6 gap-y-2 py-3 first:pt-0 last:pb-0">
                    <div className="flex min-w-32 flex-1 items-center gap-3">
                      <span className="flex size-9 items-center justify-center rounded-lg bg-primary text-sm font-semibold text-primary-foreground">
                        {provider.slice(0, 1).toUpperCase()}
                      </span>
                      <span className="leading-tight">
                        <strong className="block text-sm">{provider}</strong>
                        <small className="text-xs text-muted-foreground">{available} 個可用</small>
                      </span>
                    </div>
                    <MetaStat label="帳號" value={fmt(items2.length)} />
                    <MetaStat label="呼叫" value={fmt(callsP)} />
                    <MetaStat label="Token" value={fmtCompact(tokensP)} />
                    <span className="min-w-20 text-right text-sm">
                      <span className="block text-xs text-muted-foreground">狀態</span>
                      <strong className={available ? 'text-emerald-600' : 'text-muted-foreground'}>
                        {available ? '可用' : '無可用帳號'}
                      </strong>
                    </span>
                  </div>
                )
              })}
            </div>
          ) : (
            <Empty>尚無提供商資料</Empty>
          )}
        </PanelCard>

        <PanelCard title="帳號健康" subtitle="目前帳號狀態分布" className="lg:col-span-2">
          <HealthDonut stats={stats} />
        </PanelCard>
      </div>

      {/* Token 組成＋最近活動 */}
      <div className="grid gap-4 lg:grid-cols-2">
        <PanelCard title="Token 組成" subtitle="累計用量分布" badge={fmtCompact(tokens)}>
          {tokens ? (
            <div className="flex flex-col gap-4">
              {(
                [
                  ['輸入', input, '#3b82f6'],
                  ['輸出', output, '#10b981'],
                  ['快取', cache, '#8b5cf6'],
                ] as [string, number, string][]
              ).map(([label, value, color]) => {
                const pct = tokens ? (value / tokens) * 100 : 0
                return (
                  <div key={label} className="flex items-center gap-3">
                    <span className="flex w-28 shrink-0 items-center gap-2 text-sm">
                      <i className="size-2 rounded-full" style={{ background: color }} />
                      {label}
                    </span>
                    <strong className="w-20 shrink-0 text-right text-sm tabular-nums">{fmtCompact(value)}</strong>
                    <span className="h-2 flex-1 overflow-hidden rounded-full bg-muted">
                      <span className="block h-full rounded-full" style={{ width: `${pct}%`, background: color }} />
                    </span>
                    <small className="w-12 shrink-0 text-right text-xs tabular-nums text-muted-foreground">
                      {pct.toFixed(1)}%
                    </small>
                  </div>
                )
              })}
            </div>
          ) : (
            <Empty>尚無用量資料</Empty>
          )}
        </PanelCard>

        <PanelCard
          title="最近活動"
          subtitle="依最後使用時間排序"
          badge={
            <Link to="/admin/accounts" className="text-xs font-normal text-muted-foreground underline-offset-4 hover:underline">
              查看全部
            </Link>
          }
        >
          {(() => {
            const items3 = [...accounts]
              .sort((a, b) => (b.last_used_at || b.created_at || 0) - (a.last_used_at || a.created_at || 0))
              .slice(0, 5)
            return items3.length ? (
              <div className="flex flex-col divide-y">
                {items3.map((a) => (
                  <Link key={a.id} to="/admin/accounts" className="flex items-center gap-3 py-2.5 first:pt-0 last:pb-0">
                    <span className="size-2 shrink-0 rounded-full" style={{ background: STATUS_COLOR[a.status] }} />
                    <span className="min-w-0 flex-1 leading-tight">
                      <strong className="block truncate text-sm">{a.name || a.provider}</strong>
                      <small className="text-xs text-muted-foreground">
                        {a.provider} · {a.mode === 'jwt' ? 'JWT' : 'API Key'}
                      </small>
                    </span>
                    <span className="shrink-0 text-xs text-muted-foreground">{relativeTime(a.last_used_at || a.created_at)}</span>
                  </Link>
                ))}
              </div>
            ) : (
              <Empty>尚無帳號活動</Empty>
            )
          })()}
        </PanelCard>
      </div>

      {/* 帳號調度分布＋用量排行（原用量分析頁內容） */}
      <div className="grid gap-4 lg:grid-cols-5">
        <PanelCard title="帳號調度分布" subtitle="依請求數排序" className="lg:col-span-2">
          <Donut ranking={usage?.ranking ?? []} calls={usageCalls} />
        </PanelCard>

        <PanelCard title="帳號用量排行" subtitle="目前服務程序啟動後的累計調度" badge={fmt(usageCalls)} className="lg:col-span-3">
          <Card className="overflow-x-auto py-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>帳號</TableHead>
                  <TableHead>提供商</TableHead>
                  <TableHead className="text-right">請求</TableHead>
                  <TableHead className="text-right">失敗</TableHead>
                  <TableHead className="text-right">Token</TableHead>
                  <TableHead className="w-40">佔比</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(usage?.ranking ?? []).length ? (
                  (usage?.ranking ?? []).map((r) => {
                    const pct = usageCalls ? (r.requests / usageCalls) * 100 : 0
                    return (
                      <TableRow key={r.name}>
                        <TableCell className="font-medium">{r.name}</TableCell>
                        <TableCell>
                          <span className="rounded-full bg-emerald-100 px-2 py-0.5 text-xs text-emerald-700">{r.provider}</span>
                        </TableCell>
                        <TableCell className="text-right tabular-nums">{fmt(r.requests)}</TableCell>
                        <TableCell className="text-right tabular-nums">{fmt(r.errors)}</TableCell>
                        <TableCell className="text-right tabular-nums">{fmtCompact(r.tokens)}</TableCell>
                        <TableCell>
                          <div className="flex items-center gap-2">
                            <span className="h-1.5 flex-1 overflow-hidden rounded-full bg-muted">
                              <i className="block h-full rounded-full bg-primary" style={{ width: `${pct}%` }} />
                            </span>
                            <small className="w-11 shrink-0 text-right text-xs tabular-nums text-muted-foreground">{pct.toFixed(1)}%</small>
                          </div>
                        </TableCell>
                      </TableRow>
                    )
                  })
                ) : (
                  <TableRow>
                    <TableCell colSpan={6} className="py-8 text-center text-sm text-muted-foreground">
                      尚無帳號用量資料
                    </TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
          </Card>
        </PanelCard>
      </div>

      {/* 網關資訊 */}
      <PanelCard
        title="網關資訊"
        subtitle="目前服務端點與運行設定"
        badge={
          <span className="flex items-center gap-1.5 text-xs font-normal text-muted-foreground">
            <span className="size-1.5 rounded-full bg-emerald-500" />
            Online
          </span>
        }
      >
        <div className="flex flex-col gap-4">
          <div className="flex flex-col gap-1">
            <span className="text-xs text-muted-foreground">Messages API</span>
            <code className="truncate rounded-md bg-muted px-2.5 py-1.5 font-mono text-xs">{location.origin}/v1/messages</code>
          </div>
          <div className="grid grid-cols-3 gap-3 text-sm">
            <GatewayMeta label="API 鑑權" value={status?.gateway_key_set ? '已啟用' : '未啟用'} />
            <GatewayMeta label="額度更新" value={status?.quota_refresh_interval ? `${status.quota_refresh_interval} 秒` : '手動'} />
            <GatewayMeta
              label="資料更新"
              value={data ? new Date(data.accountsData.ts * 1000).toLocaleTimeString('zh-TW', { hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '--'}
            />
          </div>
        </div>
      </PanelCard>
    </div>
  )
}

/* 帳號健康圓環：conic-gradient 依各狀態占比上色 */
function HealthDonut({ stats }: { stats?: AccountsResponse['stats'] }) {
  const keys = ['active', 'exhausted', 'cooling', 'invalid', 'disabled'] as const
  const values = keys.map((k) => Number(stats?.[k]) || 0)
  const total = values.reduce((a, b) => a + b, 0)
  let cursor = 0
  const stops: string[] = []
  keys.forEach((key, i) => {
    const start = cursor
    cursor += total ? (values[i] / total) * 100 : 0
    if (values[i]) stops.push(`${STATUS_COLOR[key]} ${start}% ${cursor}%`)
  })
  return (
    <div className="flex items-center gap-6">
      <div
        className="relative flex size-32 shrink-0 items-center justify-center rounded-full"
        style={{ background: stops.length ? `conic-gradient(${stops.join(',')})` : '#eef2f6' }}
      >
        <div className="flex size-[86px] flex-col items-center justify-center rounded-full bg-card">
          <strong className="text-xl tabular-nums">{fmt(total)}</strong>
          <span className="text-xs text-muted-foreground">帳號</span>
        </div>
      </div>
      <div className="flex min-w-0 flex-1 flex-col gap-2 text-sm">
        {total ? (
          keys
            .map((key, i) => ({ key, value: values[i] }))
            .filter((x) => x.value)
            .map((x) => (
              <div key={x.key} className="flex items-center gap-2">
                <span className="size-2 shrink-0 rounded-full" style={{ background: STATUS_COLOR[x.key] }} />
                <span className="flex-1 text-muted-foreground">{STATUS_LABEL_LONG[x.key]}</span>
                <strong className="tabular-nums">{x.value}</strong>
              </div>
            ))
        ) : (
          <Empty>尚無帳號資料</Empty>
        )}
      </div>
    </div>
  )
}

function accountTokens(a: Account): number {
  const t = a.total_tokens || { input: 0, output: 0, cache_creation: 0, cache_read: 0 }
  return (Number(t.input) || 0) + (Number(t.output) || 0) + (Number(t.cache_creation) || 0) + (Number(t.cache_read) || 0)
}

/* 調度分布圓環配色（原用量分析頁）：前五名帳號各一色，其餘歸入灰底 */
const PALETTE = ['#3b82f6', '#10b981', '#8b5cf6', '#f59e0b', '#94a3b8']

/* 調度分布圓環：前五名帳號請求占比 */
function Donut({ ranking, calls }: { ranking: UsageResponse['ranking']; calls: number }) {
  const top = ranking.slice(0, 5)
  let cursor = 0
  const stops = top.map((r, i) => {
    const pct = calls ? (Number(r.requests) / calls) * 100 : 0
    const seg = `${PALETTE[i]} ${cursor}% ${cursor + pct}%`
    cursor += pct
    return seg
  })
  stops.push(`#e9edf3 ${cursor}% 100%`)
  return (
    <div className="flex items-center gap-6">
      <div
        className="relative flex size-32 shrink-0 items-center justify-center rounded-full"
        style={{ background: `conic-gradient(${stops.join(',')})` }}
      >
        <div className="flex size-[86px] flex-col items-center justify-center rounded-full bg-card">
          <strong className="text-xl tabular-nums">{fmt(calls)}</strong>
          <span className="text-xs text-muted-foreground">請求</span>
        </div>
      </div>
      <div className="flex min-w-0 flex-1 flex-col gap-2 text-sm">
        {top.length ? (
          top.map((r, i) => (
            <div key={r.name} className="flex items-center gap-2">
              <span className="size-2 shrink-0 rounded-full" style={{ background: PALETTE[i] }} />
              <span className="min-w-0 flex-1 truncate text-muted-foreground">{r.name}</span>
              <strong className="shrink-0 tabular-nums">{calls ? ((r.requests / calls) * 100).toFixed(1) : '0'}%</strong>
            </div>
          ))
        ) : (
          <Empty>尚無用量資料</Empty>
        )}
      </div>
    </div>
  )
}

function MetaStat({ label, value }: { label: string; value: string }) {
  return (
    <span className="text-right text-sm">
      <span className="block text-xs text-muted-foreground">{label}</span>
      <strong className="tabular-nums">{value}</strong>
    </span>
  )
}

function GatewayMeta({ label, value }: { label: string; value: string }) {
  return (
    <span className="rounded-lg bg-muted/60 px-3 py-2">
      <span className="block text-xs text-muted-foreground">{label}</span>
      <strong className="block truncate">{value}</strong>
    </span>
  )
}

