"""頁面路由：登入、控制台、用量分析、運維監控與設定。"""

from __future__ import annotations

from fastapi import APIRouter, HTTPException
from fastapi.responses import HTMLResponse, RedirectResponse

from .. import settings

router = APIRouter()

_TOKEN = "{{APP_VERSION}}"


def _html(name: str) -> HTMLResponse:
    path = settings.STATIC_DIR / "admin" / name
    if not path.exists():
        raise HTTPException(404, "页面不存在")
    body = path.read_text(encoding="utf-8").replace(_TOKEN, settings.APP_VERSION)
    return HTMLResponse(body, headers={"Cache-Control": "no-store"})


@router.get("/", include_in_schema=False)
async def root():
    return RedirectResponse("/admin")


@router.get("/admin", include_in_schema=False)
async def admin_root():
    return RedirectResponse("/admin/dashboard")


@router.get("/admin/login", include_in_schema=False)
async def admin_login():
    return _html("login.html")


@router.get("/admin/dashboard", include_in_schema=False)
async def admin_dashboard():
    return _html("dashboard.html")


@router.get("/admin/usage", include_in_schema=False)
async def admin_usage():
    return _html("usage.html")


@router.get("/admin/monitor", include_in_schema=False)
async def admin_monitor():
    return _html("monitor.html")


@router.get("/admin/accounts", include_in_schema=False)
async def admin_accounts():
    return _html("accounts.html")


@router.get("/admin/settings", include_in_schema=False)
async def admin_settings():
    return _html("settings.html")


@router.get("/admin/captcha", include_in_schema=False)
async def admin_captcha():
    return _html("captcha.html")


@router.get("/meta", include_in_schema=False)
async def meta():
    return {"version": settings.APP_VERSION}
