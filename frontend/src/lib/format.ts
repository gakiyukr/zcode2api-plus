/* 數值與時間格式化工具（對應舊版 ui.js 的共用語義） */

/* 千分位整數 */
export function fmt(v: number | null | undefined): string {
  return Number(v || 0).toLocaleString('zh-TW')
}

/* 緊湊數值：B/M 取兩位小數、k 取一位小數，其餘以千分位顯示 */
export function fmtCompact(v: number | null | undefined): string {
  const n = Number(v || 0)
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B'
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M'
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k'
  return fmt(n)
}

/* epoch 秒／毫秒 →「MM/DD HH:mm」 */
export function fmtDate(ts: number | null | undefined): string {
  if (!ts) return '—'
  // 自動偵測：數值大於 1e11 視為毫秒（對齊後端 epochSeconds），否則為秒
  const ms = ts > 1e11 ? ts : ts * 1000
  const d = new Date(ms)
  if (isNaN(d.getTime())) return '—'
  return d.toLocaleString('zh-TW', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

/* epoch 秒／毫秒 → 相對時間（最近活動用） */
export function relativeTime(ts: number | null | undefined): string {
  if (!ts) return '尚未使用'
  const epochMs = ts > 1e11 ? ts : ts * 1000
  const sec = Math.max(0, (Date.now() - epochMs) / 1000)
  if (sec < 60) return '剛剛'
  if (sec < 3600) return `${Math.floor(sec / 60)} 分鐘前`
  if (sec < 86400) return `${Math.floor(sec / 3600)} 小時前`
  return `${Math.floor(sec / 86400)} 天前`
}

/* 秒數 → 人類可讀時長（運維監控用） */
export function duration(sec: number | null | undefined): string {
  sec = Number(sec || 0)
  if (sec < 60) return `${sec} 秒`
  if (sec < 3600) return `${Math.floor(sec / 60)} 分鐘`
  if (sec < 86400) return `${Math.floor(sec / 3600)} 小時`
  return `${Math.floor(sec / 86400)} 天`
}

/* HTML 跳脫（僅用於少數需要組 HTML 字串的場景，React 下大多不需手動跳脫） */
export function esc(s: unknown): string {
  return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

/* 模型名稱正規化，供停用模型比對 */
export function normalizeModel(model: unknown): string {
  return String(model || '')
    .trim()
    .toLowerCase()
    .replace(/[_ ]/g, '-')
    .replace(/-+/g, '-')
}

/* 代理位址 → 顯示用的協定標籤 */
export function proxyScheme(url: string | null | undefined): 'SOCKS5' | 'HTTP' {
  return String(url || '').split(':')[0].toLowerCase().startsWith('socks') ? 'SOCKS5' : 'HTTP'
}
