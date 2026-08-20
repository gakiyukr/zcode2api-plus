"""Async 空闲池路由（免费低优先级计算）。

从 TriDefender/zcode-api 移植：
- 取票、SSE keepalive、等待 ready、转发响应
- 只支持 OAuth 账号（JWT）
- ticket 过期自动重试
"""

from __future__ import annotations

import asyncio
import json
import time
import uuid
from typing import AsyncIterator

import httpx
from fastapi import APIRouter, Depends, Request
from fastapi.responses import JSONResponse, StreamingResponse

from .. import logs, settings
from ..agent import build_request
from ..auth_admin import verify_gateway_key
from ..captcha import captcha_manager
from ..proxy import make_async_client
from ..store import store
from .gateway import MAX_CAPTCHA_RETRIES, _is_captcha_error, _normalize_body

router = APIRouter()

# ticket 存储：ticket_id -> {status, request_body, response_queue, created_at}
_tickets: dict[str, dict] = {}


@router.post("/async/v1/messages", dependencies=[Depends(verify_gateway_key)])
async def async_messages(request: Request):
    """创建 async ticket 并 SSE 等待结果。"""
    if not settings.ASYNC_ENABLED:
        return JSONResponse(
            {"error": {"message": "Async 路由未启用", "type": "feature_disabled"}},
            status_code=503,
        )

    try:
        body = await request.json()
    except (json.JSONDecodeError, ValueError):
        return JSONResponse(
            {"error": {"message": "请求体不是合法 JSON", "type": "invalid_request"}},
            status_code=400,
        )

    # 创建 ticket
    ticket_id = str(uuid.uuid4())
    queue: asyncio.Queue = asyncio.Queue()
    _tickets[ticket_id] = {
        "status": "pending",
        "body": body,
        "queue": queue,
        "created_at": time.monotonic(),
    }

    # 后台任务：尝试执行
    asyncio.create_task(_process_ticket(ticket_id))

    # SSE 流式返回
    return StreamingResponse(
        _ticket_sse(ticket_id),
        media_type="text/event-stream",
    )


async def _ticket_sse(ticket_id: str) -> AsyncIterator[str]:
    """SSE keepalive + 等待结果。"""
    ticket = _tickets.get(ticket_id)
    if not ticket:
        yield f"event: error\ndata: {json.dumps({'error': 'ticket not found'})}\n\n"
        return

    queue: asyncio.Queue = ticket["queue"]
    deadline = ticket["created_at"] + settings.ASYNC_TICKET_TIMEOUT

    # keepalive 心跳
    yield f"event: ticket\ndata: {json.dumps({'id': ticket_id, 'status': 'pending'})}\n\n"

    while time.monotonic() < deadline:
        try:
            event = await asyncio.wait_for(queue.get(), timeout=10)
        except asyncio.TimeoutError:
            # keepalive
            yield ": keepalive\n\n"
            continue

        if event["type"] == "ready":
            yield f"event: ready\ndata: {json.dumps({'status': 'processing'})}\n\n"
        elif event["type"] == "chunk":
            yield f"data: {json.dumps(event['data'])}\n\n"
        elif event["type"] == "done":
            yield "event: done\ndata: {}\n\n"
            break
        elif event["type"] == "error":
            yield f"event: error\ndata: {json.dumps(event['data'])}\n\n"
            break

    # 清理
    _tickets.pop(ticket_id, None)


async def _forward_sse(resp, queue) -> None:
    """把上游 SSE 行转成 ticket chunk 事件。"""
    async for line in resp.aiter_lines():
        if line.startswith("data: "):
            chunk_data = line[6:]
            if chunk_data.strip() == "[DONE]":
                continue
            try:
                await queue.put({"type": "chunk", "data": json.loads(chunk_data)})
            except json.JSONDecodeError:
                pass
    await queue.put({"type": "done"})


async def _process_ticket(ticket_id: str):
    """后台执行 ticket 请求。JWT 走主网关同一套验证码续期。"""
    ticket = _tickets.get(ticket_id)
    if not ticket:
        return

    queue: asyncio.Queue = ticket["queue"]
    body = ticket["body"]
    retries = 0
    announced_ready = False

    while retries <= settings.ASYNC_MAX_RETRIES:
        account = store.select("zai")
        if not account or account.mode != "jwt":
            await queue.put({
                "type": "error",
                "data": {"error": {"message": "无可用 OAuth 账号", "type": "no_account"}},
            })
            return

        actual_body = _normalize_body(body.copy(), needs_zcode_system=True)
        actual_payload = json.dumps(actual_body).encode("utf-8")
        network_retry = False
        last_network_error = None

        for attempt in range(MAX_CAPTCHA_RETRIES):
            try:
                captcha_token = await captcha_manager.get_verify_param()
            except Exception as err:  # noqa: BLE001
                captcha_manager.invalidate()
                if attempt + 1 < MAX_CAPTCHA_RETRIES:
                    logs.warn(ticket_id, f"验证码自动求解失败，刷新令牌重试（第 {attempt + 1} 次）")
                    continue
                await queue.put({
                    "type": "error",
                    "data": {"error": {"message": f"自动验证码暂时失败: {err}", "type": "captcha_required"}},
                })
                return

            verify_param = captcha_token.verify_param if captcha_token else None
            verify_region = captcha_token.region if captcha_token else None
            try:
                url, headers = build_request(account, actual_body, verify_param, verify_region, {})
            except Exception as exc:
                await queue.put({
                    "type": "error",
                    "data": {"error": {"message": str(exc), "type": "build_error"}},
                })
                return

            if not announced_ready:
                await queue.put({"type": "ready"})
                announced_ready = True

            try:
                async with make_async_client(account, timeout=httpx.Timeout(180)) as client:
                    async with client.stream("POST", url, headers=headers, content=actual_payload) as resp:
                        if resp.status_code == 200:
                            await _forward_sse(resp, queue)
                            return

                        text = (await resp.aread()).decode("utf-8", "ignore")
                        if _is_captcha_error(text, resp.status_code, resp.headers):
                            captcha_manager.invalidate()
                            logs.warn(ticket_id, f"账号 {account.name} 验证码失效，刷新重试（第 {attempt + 1} 次）")
                            if attempt + 1 >= MAX_CAPTCHA_RETRIES:
                                await queue.put({
                                    "type": "error",
                                    "data": {"error": {"message": "上游连续拒绝验证码", "type": "captcha_required"}},
                                })
                                return
                            continue

                        if resp.status_code in (429, 503):
                            network_retry = True
                            last_network_error = text
                            break

                        await queue.put({
                            "type": "error",
                            "data": {"error": {"message": text, "type": "upstream_error"}},
                        })
                        return
            except Exception as exc:
                logs.debug(f"async ticket {ticket_id} 请求失败: {exc}")
                network_retry = True
                last_network_error = str(exc)
                break
        else:
            await queue.put({
                "type": "error",
                "data": {"error": {"message": "上游连续拒绝验证码", "type": "captcha_required"}},
            })
            return

        if not network_retry:
            return

        retries += 1
        if retries > settings.ASYNC_MAX_RETRIES:
            await queue.put({
                "type": "error",
                "data": {"error": {"message": last_network_error or "重试次数耗尽", "type": "max_retries"}},
            })
            return
        await asyncio.sleep(2 ** retries)

    await queue.put({
        "type": "error",
        "data": {"error": {"message": "重试次数耗尽", "type": "max_retries"}},
    })


@router.post("/async/v1/chat/completions", dependencies=[Depends(verify_gateway_key)])
async def async_chat_completions(request: Request):
    """OpenAI 兼容的 async chat completions。"""
    if not settings.ASYNC_ENABLED:
        return JSONResponse(
            {"error": {"message": "Async 路由未启用", "type": "feature_disabled"}},
            status_code=503,
        )

    try:
        body = await request.json()
    except (json.JSONDecodeError, ValueError):
        return JSONResponse(
            {"error": {"message": "请求体不是合法 JSON", "type": "invalid_request"}},
            status_code=400,
        )

    # 转换为 /v1/messages 格式
    messages = body.get("messages", [])
    model = body.get("model", "GLM-5-Turbo")
    max_tokens = body.get("max_tokens", 8192)

    converted_body = {
        "model": model,
        "max_tokens": max_tokens,
        "messages": messages,
    }

    # 创建 ticket
    ticket_id = str(uuid.uuid4())
    queue: asyncio.Queue = asyncio.Queue()
    _tickets[ticket_id] = {
        "status": "pending",
        "body": converted_body,
        "queue": queue,
        "created_at": time.monotonic(),
    }

    # 后台任务
    asyncio.create_task(_process_ticket(ticket_id))

    # SSE 流式返回
    return StreamingResponse(
        _ticket_sse(ticket_id),
        media_type="text/event-stream",
    )
