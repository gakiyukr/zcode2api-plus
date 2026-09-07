/* 後台導覽定義：總覽（儀表板，含原用量分析內容）／營運／設定三分組 */
import {
  LayoutDashboard,
  Settings,
  ShieldCheck,
  SlidersHorizontal,
  Users,
  type LucideIcon,
} from 'lucide-react'

export interface NavItem {
  href: string
  label: string
  group: '總覽' | '營運' | '設定'
  icon: LucideIcon
}

export const NAV_ITEMS: NavItem[] = [
  { href: '/admin/dashboard', label: '儀表板', group: '總覽', icon: LayoutDashboard },
  { href: '/admin/accounts', label: '帳號池', group: '營運', icon: Users },
  { href: '/admin/proxies', label: '代理設定', group: '營運', icon: SlidersHorizontal },
  { href: '/admin/captcha', label: '驗證中心', group: '營運', icon: ShieldCheck },
  { href: '/admin/settings', label: '系統設定', group: '設定', icon: Settings },
]

export const NAV_GROUPS = ['總覽', '營運', '設定'] as const
