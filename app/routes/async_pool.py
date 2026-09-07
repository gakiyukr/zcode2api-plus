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
from ..models import Status
from ..store import store
from ..usage import UsageCollector
from .gateway import (
    AVAILABLE_MODELS,
    MAX_CAPTCHA_RETRIES,
    _is_captcha_error,
    _model_allowed,
    _normalize_body,
)

router = APIRouter()

# ticket 存储：ticket_id -> {status, body, queue, created_at, task}
_tickets: dict[str, dict] = {}
_TICKET_SWEEP_GRACE_SECONDS = 60.0


class _MidStreamError(Exception):
    """上游 SSE 已开始向客户端转发后中断。

    已发出的 chunk 无法撤回，此时换号重发会让客户端在同一条流里收到
    重复的 message_start 与内容块，必须终止票务而非重试。
    """


def _release_ticket(ticket_id: str) -> None:
    """移除 ticket 并中止仍在运行的后台任务。

    客户端断开或放弃后继续请求上游只会白耗账号额度，后台任务一并取消；
    任务已自然结束时 cancel() 是无操作。
    """
    ticket = _tickets.pop(ticket_id, None)
    if ticket is None:
        return
    task = ticket.get("task")
    if task is not None and not task.done():
        task.cancel()


def _sweep_expired_tickets() -> None:
    """清扫超过生命周期的孤儿 ticket（如客户端在建票后、SSE 启动前消失）。

    正常退出由 _ticket_sse 的 finally 负责清理；此处按建票时间加宽限兜底。
    """
    cutoff = time.monotonic() - settings.ASYNC_TICKET_TIMEOUT - _TICKET_SWEEP_GRACE_SECONDS
    stale = [tid for tid, t in _tickets.items() if t["created_at"] < cutoff]
    for ticket_id in stale:
        _release_ticket(ticket_id)


def _new_ticket(body: dict) -> str:
    """创建 ticket 并启动后台任务，返回 ticket_id。"""
    _sweep_expired_tickets()
    ticket_id = str(uuid.uuid4())
    _tickets[ticket_id] = {
        "status": "pending",
        "body": body,
        "queue": asyncio.Queue(),
        "created_at": time.monotonic(),
    }
    _tickets[ticket_id]["task"] = asyncio.create_task(_process_ticket(ticket_id))
    return ticket_id


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

    # 模型白名單與 /v1/messages 一致：僅開放清單內模型，其餘在建票前一律拒絕
    if not _model_allowed(body.get("model")):
        model_name = str(body.get("model") or "")
        logs.warn("async", f"模型 {model_name} 不在開放清單內，拒絕建票")
        return JSONResponse(
            {"error": {"message": f"模型 {model_name} 不在可用清單內，僅支持 {', '.join(AVAILABLE_MODELS)}", "type": "model_not_allowed"}},
            status_code=400,
        )

    # 创建 ticket 并启动后台任务
    ticket_id = _new_ticket(body)

    # SSE 流式返回
    return StreamingResponse(
        _ticket_sse(ticket_id),
        media_type="text/event-stream",
    )


async def _ticket_sse(ticket_id: str) -> AsyncIterator[str]:
    """SSE keepalive + 等待结果；退出时（含客户端断开）释放 ticket 并中止后台任务。"""
    ticket = _tickets.get(ticket_id)
    if not ticket:
        yield f"event: error\ndata: {json.dumps({'error': 'ticket not found'})}\n\n"
        return

    queue: asyncio.Queue = ticket["queue"]
    deadline = ticket["created_at"] + settings.ASYNC_TICKET_TIMEOUT

    try:
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
    finally:
        # 客户端断开时生成器被关闭，finally 是唯一必经的清理点
        _release_ticket(ticket_id)


async def _forward_sse(resp, queue, account) -> None:
    """把上游 SSE 行转成 ticket chunk 事件，并累计賬號 token 用量。

    转发开始后上游中断：已发出过 chunk 时抛 _MidStreamError（调用方终止票务），
    一个 chunk 都没发过时按普通网络错误抛出（调用方换号重试）。
    """
    usage = UsageCollector(is_sse=True)
    chunks_sent = 0
    try:
        async for line in resp.aiter_lines():
            usage.feed_line(line)
            if line.startswith("data: "):
                chunk_data = line[6:]
                if chunk_data.strip() == "[DONE]":
                    continue
                try:
                    payload = json.loads(chunk_data)
                except json.JSONDecodeError:
                    continue
                await queue.put({"type": "chunk", "data": payload})
                chunks_sent += 1
    except Exception as exc:
        if chunks_sent:
            raise _MidStreamError(str(exc) or type(exc).__name__) from exc
        raise
    try:
        usage.finish()
        account.accumulate_tokens(usage.as_dict())
        store.update_account(account)
    except Exception as exc:  # noqa: BLE001 - 统计落库失败不应触发换号重发
        logs.warn("async", f"用量统计落库失败: {exc}")
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
    tried: set[str] = set()

    while retries <= settings.ASYNC_MAX_RETRIES:
        account = store.select("zai", skip_ids=tried, model=body.get("model"))
        if not account or account.mode != "jwt":
            await queue.put({
                "type": "error",
                "data": {"error": {"message": "无可用 OAuth 账号", "type": "no_account"}},
            })
            return
        tried.add(account.id)

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
                            await _forward_sse(resp, queue, account)
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
                            account.fail_count += 1
                            account.status = Status.COOLING
                            account.cooling_until = time.time() + settings.COOLING_SECONDS
                            account.last_error = f"上游服務暫時不可用 HTTP {resp.status_code}"
                            store.update_account(account)
                            network_retry = True
                            last_network_error = text
                            break

                        await queue.put({
                            "type": "error",
                            "data": {"error": {"message": text, "type": "upstream_error"}},
                        })
                        return
            except _MidStreamError as exc:
                # 已向客户端发出内容块，不能换号重发（会收到重复事件），终止本票
                logs.warn(ticket_id, f"流转发中断: {exc}")
                await queue.put({
                    "type": "error",
                    "data": {"error": {"message": f"上游流式响应中断: {exc}", "type": "upstream_stream_interrupted"}},
                })
                return
            except Exception as exc:
                logs.warn(ticket_id, f"请求失败: {exc}")
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
