import time
from dataclasses import dataclass
from datetime import datetime
from http import HTTPStatus

from fastapi.responses import JSONResponse
from starlette.datastructures import Headers
from starlette.types import ASGIApp, Receive, Scope, Send

from .db import Pool, fetch_all
from .timeutil import now

RELOAD_INTERVAL = 0.2


@dataclass(frozen=True, slots=True)
class IPBanSnapshot:
    loaded_at: float
    bans: dict[str, datetime]


class IPBanList:
    def __init__(self) -> None:
        self.snapshot: IPBanSnapshot | None = None
        self.reloading = False

    async def banned(self, pool: Pool, ip: str) -> bool:
        snapshot = self.snapshot
        stale = (
            snapshot is None or time.monotonic() - snapshot.loaded_at >= RELOAD_INTERVAL
        )
        if stale and not self.reloading:
            self.reloading = True
            try:
                snapshot = self.snapshot = await load_ip_bans(pool)
            finally:
                self.reloading = False
        if snapshot is None:
            return False
        until = snapshot.bans.get(ip)
        return until is not None and now() < until


async def load_ip_bans(pool: Pool) -> IPBanSnapshot:
    async with pool.connection() as db:
        rows = await fetch_all(
            db,
            "SELECT ip, banned_until FROM ip_bans WHERE banned_until > clock_timestamp()",
        )
    return IPBanSnapshot(
        loaded_at=time.monotonic(),
        bans={row["ip"]: row["banned_until"] for row in rows},
    )


def client_ip(scope: Scope) -> str:
    if ip := Headers(scope=scope).get("x-client-ip"):
        return ip
    client = scope.get("client")
    return client[0] if client else ""


class IPBanMiddleware:
    def __init__(self, app: ASGIApp) -> None:
        self.app = app
        self.ip_bans = IPBanList()

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] == "http" and await self.ip_bans.banned(
            scope["app"].state.pool, client_ip(scope)
        ):
            response = JSONResponse(
                {"error": "ip_banned"}, status_code=HTTPStatus.FORBIDDEN
            )
            await response(scope, receive, send)
            return
        await self.app(scope, receive, send)
