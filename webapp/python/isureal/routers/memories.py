from datetime import date, datetime, timedelta
from http import HTTPStatus
from typing import Annotated

from fastapi import APIRouter, Query

from ..auth import RegionalSession
from ..binding import cursor_time
from ..db import DB, Connection
from ..errors import APIError
from ..models.comment import select_comments_with_author
from ..models.post import select_posts_with_author
from ..models.realmoji import select_current_realmojis
from ..schemas import CommentResponse, MemoryPostResponse, Page, RealMojiResponse
from ..timeutil import add_months, parse_date, parse_month

router = APIRouter(prefix="/api/memories")

type BeforePostedAt = Annotated[str | None, Query(alias="beforePostedAt")]
type BeforeID = Annotated[str, Query(alias="beforeId")]


def invalid_memory_period() -> APIError:
    return APIError(HTTPStatus.BAD_REQUEST, "invalid_memory_period")


@router.get("")
async def get_memories(
    db: DB,
    session: RegionalSession,
    month: str = "",
    before_posted_at: BeforePostedAt = None,
    before_id: BeforeID = "",
) -> Page[MemoryPostResponse]:
    before_at = cursor_time(before_posted_at)
    start = parse_month(month)
    if start is None:
        raise invalid_memory_period()
    return await list_memories(
        db, session.user_id, start, add_months(start, 1), before_at, before_id
    )


@router.get("/{day}")
async def get_memories_by_date(
    db: DB,
    session: RegionalSession,
    day: str,
    before_posted_at: BeforePostedAt = None,
    before_id: BeforeID = "",
) -> Page[MemoryPostResponse]:
    before_at = cursor_time(before_posted_at)
    start = parse_date(day)
    if start is None:
        raise invalid_memory_period()
    return await list_memories(
        db, session.user_id, start, start + timedelta(days=1), before_at, before_id
    )


async def list_memories(
    db: Connection,
    user_id: str,
    start: date,
    end: date,
    before_at: datetime | None,
    before_id: str,
) -> Page[MemoryPostResponse]:
    if (before_at is None) != (before_id == ""):
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_pagination")
    posts = await select_posts_with_author(
        db,
        """
        AND p.author_id = %(user_id)s AND w.local_date >= %(start)s AND w.local_date < %(end)s
        AND (%(before_at)s::timestamptz IS NULL OR (p.posted_at, p.id) < (%(before_at)s, %(before_id)s))
        ORDER BY p.posted_at DESC, p.id DESC LIMIT 51
        """,
        {
            "user_id": user_id,
            "start": start,
            "end": end,
            "before_at": before_at,
            "before_id": before_id,
        },
    )
    page = Page.of([MemoryPostResponse.of(post, author) for post, author in posts])
    await load_memory_interactions(db, page.items)
    return page


async def load_memory_interactions(
    db: Connection, items: list[MemoryPostResponse]
) -> None:
    if not items:
        return
    item_by_post_id = {item.id: item for item in items}
    params = {"post_ids": list(item_by_post_id)}
    for reaction, reactor in await select_current_realmojis(
        db,
        "AND r.post_id = ANY(%(post_ids)s) ORDER BY r.created_at DESC, r.id DESC",
        params,
    ):
        item = item_by_post_id[reaction.post_id]
        if len(item.realmojis) < 50:
            item.realmojis.append(RealMojiResponse.of(reaction, reactor))
    for comment, author in await select_comments_with_author(
        db, "AND c.post_id = ANY(%(post_ids)s) ORDER BY c.created_at, c.id", params
    ):
        item = item_by_post_id[comment.post_id]
        if len(item.comments) < 50:
            item.comments.append(CommentResponse.of(comment, author))
