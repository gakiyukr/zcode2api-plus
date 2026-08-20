"""Async (off-peak) 类型定义。"""

from __future__ import annotations

from typing import Literal

# Ticket 生命周期状态
TicketState = Literal["queued", "ready", "active", "settled", "expired", "not_found"]

# 非终止状态
TICKET_PENDING_STATES = ("queued",)
# 终止状态
TICKET_TERMINAL_STATES = ("settled", "expired", "not_found")


def is_ticket_ready(state: TicketState) -> bool:
    """判断 ticket 是否就绪可用。"""
    return state in ("ready", "active")


def is_ticket_expired(state: TicketState) -> bool:
    """判断 ticket 是否已过期。"""
    return state in ("expired", "not_found")


class OffPeakServerError(Exception):
    """上游 off-peak 服务器错误。"""
    def __init__(self, message: str, http_status: int, biz_code: str | None = None):
        super().__init__(message)
        self.http_status = http_status
        self.biz_code = biz_code


class OffPeakCredentialsUnavailableError(Exception):
    """Off-peak 需要 OAuth 凭证（JWT），但当前账号不支持。"""
    pass


def is_off_peak_ticket_expired_error(error: Exception | str) -> bool:
    """检测是否为 ticket 过期错误。"""
    msg = str(error)
    return "off-peak-ticket-expired" in msg or "ticket expired" in msg.lower()
