"""Off-peak ticket-queue HTTP 客户端。

实现 4 个控制端点：
  - GET  /ticket/availability
  - POST /ticket
  - POST /ticket/status
  - POST /ticket/{id}/settle
"""

from __future__ import annotations

import asyncio
import json
from typing import Any

import httpx

from ..proxy import make_async_client
from .types import OffPeakServerError, TicketState


class OffPeakClient:
    """Off-peak ticket-queue 客户端。"""
    
    def __init__(
        self,
        origin: str,
        jwt: str,
        coding_plan_api_key: str,
        control_timeout_ms: int = 15000,
        settle_timeout_ms: int | None = None,
        proxy_url: str | None = None,
    ):
        self.origin = origin.rstrip("/")
        self.jwt = jwt
        self.coding_plan_api_key = coding_plan_api_key
        self.control_timeout_ms = control_timeout_ms
        self.settle_timeout_ms = settle_timeout_ms or control_timeout_ms
        self.proxy_url = proxy_url
    
    def _build_headers(self, has_body: bool = False) -> dict[str, str]:
        headers = {
            "authorization": f"Bearer {self.jwt}",
            "x-coding-plan-api-key": self.coding_plan_api_key,
        }
        if has_body:
            headers["content-type"] = "application/json"
        return headers
    
    async def _request(
        self,
        method: str,
        path: str,
        body: dict | None = None,
        timeout_ms: int | None = None,
        settle_as_success: bool = False,
    ) -> dict[str, Any]:
        """发起请求并解析 JSON 响应。"""
        url = f"{self.origin}/api/v1/off-peak{path}"
        headers = self._build_headers(body is not None)
        timeout = (timeout_ms or self.control_timeout_ms) / 1000.0
        
        async with make_async_client(proxy_url=self.proxy_url, timeout=httpx.Timeout(timeout)) as client:
            try:
                if body:
                    resp = await client.request(method, url, headers=headers, json=body)
                else:
                    resp = await client.request(method, url, headers=headers)
            except httpx.TimeoutException as err:
                raise OffPeakServerError(f"请求超时: {err}", 408) from err
            except httpx.RequestError as err:
                raise OffPeakServerError(f"请求失败: {err}", 500) from err
            
            # settle 操作的 4xx 视为成功（服务端已清理）
            if settle_as_success and 400 <= resp.status_code < 500:
                return {}
            
            if resp.status_code >= 400:
                try:
                    error_data = resp.json()
                    message = error_data.get("message", resp.text)
                    code = error_data.get("code")
                except Exception:
                    message = resp.text
                    code = None
                raise OffPeakServerError(message, resp.status_code, code)
            
            try:
                return resp.json()
            except Exception:
                return {}
    
    async def get_availability(self) -> dict[str, Any]:
        """GET /ticket/availability - 查询队列可用性。"""
        data = await self._request("GET", "/ticket/availability")
        return {
            "can_take_number": data.get("can_take_number", True),
            "next_take_at": data.get("next_take_at"),
        }
    
    async def take_ticket(self, task_id: str) -> dict[str, Any]:
        """POST /ticket - 取号排队。"""
        data = await self._request("POST", "/ticket", {"task_id": task_id})
        return {
            "ticket_id": data.get("ticket_id", ""),
            "state": data.get("state", "queued"),
            "position": data.get("position"),
            "next_poll_after_ms": data.get("next_poll_after"),
            "registered_at": data.get("registered_at", 0),
        }
    
    async def batch_status(self, ticket_ids: list[str]) -> dict[str, Any]:
        """POST /ticket/status - 批量查询 ticket 状态。"""
        # 限制单次最多 100 个
        ticket_ids = ticket_ids[:100]
        data = await self._request("POST", "/ticket/status", {"ticket_ids": ticket_ids})
        
        tickets = []
        for t in data.get("tickets", []):
            tickets.append({
                "ticket_id": t.get("ticket_id", ""),
                "state": t.get("state", "not_found"),
                "position": t.get("position"),
                "active_deadline": t.get("active_deadline"),
            })
        
        return {
            "next_poll_after_ms": data.get("next_poll_after"),
            "tickets": tickets,
        }
    
    async def settle(self, ticket_id: str, settle_as_success: bool = True) -> None:
        """POST /ticket/{id}/settle - 结算 ticket。"""
        await self._request(
            "POST",
            f"/ticket/{ticket_id}/settle",
            timeout_ms=self.settle_timeout_ms,
            settle_as_success=settle_as_success,
        )


def create_off_peak_client(
    origin: str,
    jwt: str,
    coding_plan_api_key: str,
    control_timeout_ms: int = 15000,
    settle_timeout_ms: int | None = None,
    proxy_url: str | None = None,
) -> OffPeakClient:
    """创建 OffPeakClient 实例。"""
    return OffPeakClient(
        origin=origin,
        jwt=jwt,
        coding_plan_api_key=coding_plan_api_key,
        control_timeout_ms=control_timeout_ms,
        settle_timeout_ms=settle_timeout_ms,
        proxy_url=proxy_url,
    )
