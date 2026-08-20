"""Async (off-peak) 路由处理器。

实现两个端点：
  - POST /async/v1/messages          (Anthropic 格式)
  - POST /async/v1/chat/completions  (OpenAI 格式)

工作流程：
1. 验证账号是否支持 OAuth (有 JWT)
2. 取号排队 (takeTicket)
3. 轮询状态直到 ready
4. 发送 SSE keepalive 保持连接
5. ticket ready 后转发请求
6. 流式返回响应
7. 完成后 settle ticket
"""

from __future__ import annotations

import asyncio
import json
import secrets
import time
from typing import AsyncGenerator

from fastapi import APIRouter, Depends, Request
from fastapi.responses import JSONResponse, StreamingResponse

from .. import logs, settings
from ..agent import build_request
from ..auth_admin import verify_gateway_key
from ..models import Account
from ..proxy import make_async_client
from ..store import store
from .client import create_off_peak_client
from .types import (
    OffPeakCredentialsUnavailableError,
    OffPeakServerError,
    is_off_peak_ticket_expired_error,
    is_ticket_ready,
)

router = APIRouter()

# Async 配置
ASYNC_ORIGIN = "https://zcode.z.ai"
ASYNC_POLL_INTERVAL_MS = 5000
ASYNC_KEEPALIVE_INTERVAL_MS = 3000
ASYNC_MAX_WAIT_MS = 0  # 0 = 无限制
ASYNC_MAX_RETRIES = 3
ASYNC_SETTLE_TIMEOUT_MS = 8000
ASYNC_CONTROL_TIMEOUT_MS = 15000


def _generate_task_id() -> str:
    """生成任务 ID。"""
    return f"proxy-{int(time.time() * 1000)}-{secrets.token_hex(4)}"


async def _wait_for_ready(
    client,
    ticket_id: str,
    keepalive_callback,
    max_wait_ms: int = 0,
) -> bool:
    """等待 ticket 变为 ready 状态。
    
    Args:
        client: OffPeakClient 实例
        ticket_id: ticket ID
        keepalive_callback: 定期调用以发送 keepalive
        max_wait_ms: 最大等待时间（毫秒），0 表示无限制
    
    Returns:
        True 表示 ticket ready，False 表示超时或失败
    """
    start_time = time.time()
    poll_interval = ASYNC_POLL_INTERVAL_MS / 1000.0
    
    while True:
        # 检查超时
        if max_wait_ms > 0:
            elapsed_ms = (time.time() - start_time) * 1000
            if elapsed_ms >= max_wait_ms:
                return False
        
        # 查询状态
        try:
            result = await client.batch_status([ticket_id])
            tickets = result.get("tickets", [])
            if not tickets:
                return False
            
            ticket = tickets[0]
            state = ticket.get("state", "not_found")
            
            if is_ticket_ready(state):
                return True
            
            if state in ("settled", "expired", "not_found"):
                return False
            
            # 发送 keepalive
            if keepalive_callback:
                await keepalive_callback()
            
            # 等待下一次轮询
            await asyncio.sleep(poll_interval)
        
        except Exception as err:
            logs.warn(f"Ticket status poll failed: {err}")
            await asyncio.sleep(poll_interval)


async def _handle_async_request(
    req_id: str,
    account: Account,
    body: dict,
    incoming_headers: dict,
    max_retries: int = ASYNC_MAX_RETRIES,
) -> StreamingResponse | JSONResponse:
    """处理异步请求（带自动重试）。"""
    
    # 验证账号支持 OAuth
    if account.mode != "jwt" or not account.jwt_token:
        raise OffPeakCredentialsUnavailableError("Async routes require OAuth account (JWT)")
    
    # 创建 off-peak 客户端
    client = create_off_peak_client(
        origin=ASYNC_ORIGIN,
        jwt=account.jwt_token,
        coding_plan_api_key=account.api_key or "",
        control_timeout_ms=ASYNC_CONTROL_TIMEOUT_MS,
        settle_timeout_ms=ASYNC_SETTLE_TIMEOUT_MS,
        proxy_url=account.proxy_url,
    )
    
    for attempt in range(max_retries):
        try:
            return await _try_async_request(req_id, account, body, incoming_headers, client)
        except Exception as err:
            if is_off_peak_ticket_expired_error(err) and attempt + 1 < max_retries:
                logs.warn(f"{req_id} Ticket expired, retrying ({attempt + 1}/{max_retries})")
                await asyncio.sleep(1)
                continue
            raise
    
    return JSONResponse(
        {"error": {"message": "Max retries exceeded", "type": "off_peak_error"}},
        status_code=503,
    )


async def _try_async_request(
    req_id: str,
    account: Account,
    body: dict,
    incoming_headers: dict,
    client,
) -> StreamingResponse:
    """单次异步请求尝试。"""
    
    # 取号
    task_id = _generate_task_id()
    logs.info(f"{req_id} Taking ticket (task_id={task_id})")
    
    try:
        ticket_result = await client.take_ticket(task_id)
    except OffPeakServerError as err:
        logs.req_err(req_id, f"Take ticket failed: {err}")
        return JSONResponse(
            {"error": {"message": str(err), "type": "off_peak_error"}},
            status_code=err.http_status,
        )
    
    ticket_id = ticket_result.get("ticket_id", "")
    if not ticket_id:
        return JSONResponse(
            {"error": {"message": "No ticket_id returned", "type": "off_peak_error"}},
            status_code=500,
        )
    
    logs.info(f"{req_id} Ticket={ticket_id}, waiting for ready...")
    
    # SSE 流生成器
    async def _stream_generator() -> AsyncGenerator[bytes, None]:
        # 发送 SSE keepalive
        async def keepalive():
            yield b": keepalive\n\n"
        
        # 等待 ticket ready
        ready = await _wait_for_ready(
            client,
            ticket_id,
            lambda: keepalive().__anext__(),
            max_wait_ms=ASYNC_MAX_WAIT_MS,
        )
        
        if not ready:
            logs.req_err(req_id, f"Ticket {ticket_id} not ready (timeout or failed)")
            yield b'data: {"error":{"message":"Ticket not ready","type":"off_peak_timeout"}}\n\n'
            try:
                await client.settle(ticket_id)
            except Exception:
                pass
            return
        
        logs.info(f"{req_id} Ticket {ticket_id} ready, forwarding request")
        
        # 构建上游请求
        try:
            url, headers = build_request(account, body, None, None, incoming_headers)
        except RuntimeError as err:
            logs.req_err(req_id, f"Build request failed: {err}")
            yield f'data: {{"error":{{"message":"{err}","type":"invalid_request"}}}}\n\n'.encode()
            try:
                await client.settle(ticket_id)
            except Exception:
                pass
            return
        
        # 发送请求并流式转发
        import httpx
        http_client = make_async_client(account, timeout=httpx.Timeout(connect=30.0, read=None, write=120.0))
        payload = json.dumps(body).encode("utf-8")
        
        try:
            async with http_client.stream("POST", url, headers=headers, content=payload) as resp:
                if resp.status_code >= 400:
                    error_text = await resp.aread()
                    logs.req_err(req_id, f"Upstream error {resp.status_code}")
                    yield f'data: {{"error":{{"message":"{error_text.decode("utf-8", "ignore")[:200]}","type":"upstream_error"}}}}\n\n'.encode()
                else:
                    async for chunk in resp.aiter_bytes():
                        yield chunk
                    logs.req_ok(req_id)
        except Exception as err:
            logs.req_err(req_id, f"Streaming failed: {err}")
            yield f'data: {{"error":{{"message":"{err}","type":"stream_error"}}}}\n\n'.encode()
        finally:
            await http_client.aclose()
            # Settle ticket
            try:
                await client.settle(ticket_id)
                logs.info(f"{req_id} Ticket {ticket_id} settled")
            except Exception as err:
                logs.warn(f"{req_id} Settle failed: {err}")
    
    return StreamingResponse(
        _stream_generator(),
        media_type="text/event-stream",
        headers={"Cache-Control": "no-cache"},
    )


@router.post("/async/v1/messages", dependencies=[Depends(verify_gateway_key)])
async def async_messages(request: Request):
    """Async (off-peak) Anthropic Messages 端点。"""
    if not settings.ASYNC_ENABLED:
        return JSONResponse(
            {"error": {"message": "Async routes disabled", "type": "feature_disabled"}},
            status_code=503,
        )
    
    try:
        body = await request.json()
    except Exception:
        return JSONResponse(
            {"error": {"message": "Invalid JSON", "type": "invalid_request"}},
            status_code=400,
        )
    
    req_id = secrets.token_hex(3)
    logs.req(req_id, str(body.get("model", "-")), True, "async")
    
    # 选择支持 OAuth 的账号
    account = store.select("zai", skip_ids=set())
    if not account or account.mode != "jwt":
        return JSONResponse(
            {"error": {"message": "No OAuth account available for async", "type": "no_oauth_account"}},
            status_code=503,
        )
    
    try:
        return await _handle_async_request(req_id, account, body, dict(request.headers))
    except OffPeakCredentialsUnavailableError as err:
        return JSONResponse(
            {"error": {"message": str(err), "type": "async_credentials_unavailable"}},
            status_code=400,
        )
    except Exception as err:
        logs.req_err(req_id, f"Async handler failed: {err}")
        return JSONResponse(
            {"error": {"message": str(err), "type": "internal_error"}},
            status_code=500,
        )


@router.post("/async/v1/chat/completions", dependencies=[Depends(verify_gateway_key)])
async def async_chat_completions(request: Request):
    """Async (off-peak) OpenAI Chat Completions 端点（转换为 Anthropic 格式）。"""
    if not settings.ASYNC_ENABLED:
        return JSONResponse(
            {"error": {"message": "Async routes disabled", "type": "feature_disabled"}},
            status_code=503,
        )
    
    return JSONResponse(
        {"error": {"message": "OpenAI format not yet implemented", "type": "not_implemented"}},
        status_code=501,
    )
