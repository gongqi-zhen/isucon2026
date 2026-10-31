import asyncio
import logging
from collections.abc import AsyncIterator
from datetime import datetime

from fastapi import APIRouter, Request
from fastapi.responses import StreamingResponse

from ..auth import RegionalSession
from ..db import DB, Pool, fetch_one, fetch_value
from ..models.session import Session
from ..models.user import get_user
from ..schemas import NotificationResponse

router = APIRouter(prefix="/api/notifications")
logger = logging.getLogger(__name__)

POLL_INTERVAL = 0.5


async def notification_events(
    pool: Pool, session: Session, connected_at: datetime
) -> AsyncIterator[bytes]:
    """接続後に届いた通知を 1 件送って終わる"""
    while True:
        try:
            async with pool.connection() as db:
                user = await get_user(db, session.user_id)
                # 退会やリージョン変更で届け先がなくなったら終わる
                if user is None or user.region != session.region:
                    return
                row = await fetch_one(
                    db,
                    """
                    SELECT n.id, n.posting_window_id, w.region, n.created_at
                    FROM notifications n JOIN posting_windows w ON w.id = n.posting_window_id
                    WHERE n.user_id = %(user_id)s AND n.created_at > %(connected_at)s
                      AND w.ended_at IS NULL
                    ORDER BY n.created_at, n.id LIMIT 1
                    """,
                    {"user_id": session.user_id, "connected_at": connected_at},
                )
        except Exception:
            logger.exception("failed to get notification")
            return
        if row is not None:
            notification = NotificationResponse(**row)
            yield b"data: " + notification.model_dump_json().encode() + b"\n\n"
            return
        await asyncio.sleep(POLL_INTERVAL)


@router.get("")
async def get_notifications(
    db: DB, session: RegionalSession, request: Request
) -> StreamingResponse:
    connected_at: datetime = await fetch_value(db, "SELECT clock_timestamp()")
    return StreamingResponse(
        notification_events(request.app.state.pool, session, connected_at),
        media_type="text/event-stream",
    )
