/* 後台共用導覽：側欄在桌面常駐，在手機以抽屜方式開啟。 */
async function renderAdminHeader(){
  const mount=document.getElementById('admin-header');
  if(!mount)return;
  let version='';
  try{const r=await fetch('/meta');if(r.ok)version='v'+(await r.json()).version;}catch{}
  const active=mount.dataset.active||location.pathname;
  const nav=[
    {href:'/admin/dashboard',label:'儀表板',group:'總覽',icon:'<rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/>'},
    {href:'/admin/usage',label:'用量分析',group:'總覽',icon:'<path d="M4 19V5M4 19h16"/><path d="m7 15 3-4 3 2 5-7"/>'},
    {href:'/admin/monitor',label:'運維監控',group:'營運',icon:'<path d="M4 19V5M4 19h16"/><path d="M8 16v-4M12 16V8M16 16v-7"/>'},
    {href:'/admin/accounts',label:'帳號池',group:'營運',icon:'<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75"/>'},
    {href:'/admin/proxies',label:'代理設定',group:'營運',icon:'<path d="M4 7h16M4 12h16M4 17h16"/><circle cx="8" cy="7" r="2"/><circle cx="15" cy="12" r="2"/><circle cx="11" cy="17" r="2"/>'},
    {href:'/admin/captcha',label:'驗證中心',group:'營運',icon:'<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M8 9h8M8 13h5M8 17h3"/>'},
    {href:'/admin/settings',label:'系統設定',group:'設定',icon:'<path d="M12 15.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7Z"/><path d="m19.4 15 .1.1a2 2 0 0 1-2.8 2.8l-.1-.1a2 2 0 0 0-3.4 1.4v.2a2 2 0 0 1-4 0v-.2a2 2 0 0 0-3.4-1.4l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1A2 2 0 0 0 3.7 11H3.5a2 2 0 0 1 0-4h.2a2 2 0 0 0 1.4-3.4L5 3.5a2 2 0 1 1 2.8-2.8l.1.1A2 2 0 0 0 11.3 1V.8a2 2 0 0 1 4 0V1a2 2 0 0 0 3.4 1.4l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1A2 2 0 0 0 22.9 8h.2a2 2 0 0 1 0 4h-.2a2 2 0 0 0-3.5 3Z"/>'}
  ];
  const groups=[...new Set(nav.map(item=>item.group))];
  const navHtml=groups.map(group=>`
    <div class="sidebar-group">
      <div class="sidebar-label">${group}</div>
      <nav class="sidebar-nav">${nav.filter(item=>item.group===group).map(item=>`
        <a href="${item.href}" class="sidebar-link${item.href===active?' active':''}">
          <svg viewBox="0 0 24 24" aria-hidden="true">${item.icon}</svg><span>${item.label}</span>
        </a>`).join('')}</nav>
    </div>`).join('');
  const pageName=(nav.find(item=>item.href===active)||{}).label||'管理後台';
  mount.innerHTML=`
    <div class="sidebar-backdrop" id="sidebar-backdrop" onclick="closeAdminSidebar()"></div>
    <aside class="admin-sidebar" id="admin-sidebar">
      <div class="sidebar-brand">
        <span class="brand-mark"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m12 3 9 4.5-9 4.5-9-4.5L12 3Z"/><path d="m3 12 9 4.5 9-4.5M3 16.5 12 21l9-4.5"/></svg></span>
        <span><strong>zcode2api</strong><small>PLUS CONSOLE</small></span>
      </div>
      <div class="sidebar-nav-wrap">${navHtml}</div>
      <div class="sidebar-footer">
        <div class="sidebar-runtime"><span class="runtime-dot"></span><span><strong>服務運行中</strong><small>API Gateway online</small></span></div>
        <button class="sidebar-logout" onclick="adminLogout()" title="登出" aria-label="登出"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><path d="m16 17 5-5-5-5M21 12H9"/></svg></button>
      </div>
    </aside>
    <header class="admin-topbar">
      <div class="topbar-left">
        <button class="sidebar-toggle" onclick="toggleAdminSidebar()" title="開啟導覽" aria-label="開啟導覽"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 6h16M4 12h16M4 18h16"/></svg></button>
        <div class="breadcrumb"><span>控制台</span><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m9 18 6-6-6-6"/></svg><strong>${pageName}</strong></div>
      </div>
      <div class="topbar-right">
        <span class="topbar-status"><span class="runtime-dot"></span>系統正常</span>
        ${version?`<span class="topbar-version">${version}</span>`:''}
        <button class="topbar-avatar" title="管理員" aria-label="管理員">Z</button>
      </div>
    </header>`;
}

function toggleAdminSidebar(){
  document.body.classList.toggle('sidebar-open');
}
function closeAdminSidebar(){
  document.body.classList.remove('sidebar-open');
}
