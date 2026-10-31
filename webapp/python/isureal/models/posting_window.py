from dataclasses import dataclass
from datetime import date, datetime

from ..db import Connection, fetch_one, from_row


@dataclass(slots=True)
class PostingWindow:
    id: str
    region: str
    local_date: date
    starts_at: datetime
    on_time_deadline: datetime
    ended_at: datetime | None


async def get_posting_window(db: Connection, window_id: str) -> PostingWindow | None:
    row = await fetch_one(
        db,
        """
        SELECT id, region, local_date, starts_at, on_time_deadline, ended_at
        FROM posting_windows WHERE id = %(id)s
        """,
        {"id": window_id},
    )
    return from_row(PostingWindow, row) if row else None
