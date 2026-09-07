/* 應用路由表：登入頁獨立於主框架，其餘頁面經 AuthGuard 後套用側欄版面 */
import { Navigate, Outlet, Route, Routes } from 'react-router-dom'
import { AuthGuard } from '@/components/layout/auth-guard'
import { AppSidebar } from '@/components/layout/app-sidebar'
import { Topbar } from '@/components/layout/topbar'
import { SidebarInset, SidebarProvider } from '@/components/ui/sidebar'
import { LoginPage } from '@/pages/login'
import { DashboardPage } from '@/pages/dashboard'
import { AccountsPage } from '@/pages/accounts'
import { ProxiesPage } from '@/pages/proxies'
import { CaptchaPage } from '@/pages/captcha'
import { SettingsPage } from '@/pages/settings'

/* 主框架：驗證通過才掛側欄與頂欄，頁面內容由巢狀路由提供 */
function Shell() {
  return (
    <AuthGuard>
      <SidebarProvider>
        <AppSidebar />
        <SidebarInset>
          <Topbar />
          <main className="flex-1 p-4 md:p-6">
            <Outlet />
          </main>
        </SidebarInset>
      </SidebarProvider>
    </AuthGuard>
  )
}

export default function App() {
  return (
    <Routes>
      <Route path="/" element={<Navigate to="/admin/dashboard" replace />} />
      <Route path="/admin/login" element={<LoginPage />} />
      <Route element={<Shell />}>
        <Route path="/admin" element={<Navigate to="/admin/dashboard" replace />} />
        <Route path="/admin/dashboard" element={<DashboardPage />} />
        <Route path="/admin/accounts" element={<AccountsPage />} />
        <Route path="/admin/proxies" element={<ProxiesPage />} />
        <Route path="/admin/captcha" element={<CaptchaPage />} />
        <Route path="/admin/settings" element={<SettingsPage />} />
        {/* SPA 內部路由後備：未知 /admin/* 路徑一律回首頁，重新整理不落 404 */}
        <Route path="/admin/*" element={<Navigate to="/admin/dashboard" replace />} />
      </Route>
      <Route path="*" element={<Navigate to="/admin/dashboard" replace />} />
    </Routes>
  )
}
