from dataclasses import dataclass
from datetime import datetime

from ..db import Connection, Params, fetch_all, fetch_one, from_row
from .post import get_post_with_author, load_viewer
from .user import User, user_columns

COMMENT_COLUMNS = "c.id, c.post_id, c.author_id, c.body, c.created_at, c.deleted_at"


@dataclass(slots=True)
class Comment:
    id: str
    post_id: str
    author_id: str
    body: str
    created_at: datetime
    deleted_at: datetime | None


async def get_comment(db: Connection, comment_id: str) -> Comment | None:
    row = await fetch_one(
        db,
        f"SELECT {COMMENT_COLUMNS} FROM comments c WHERE c.id = %(id)s",
        {"id": comment_id},
    )
    return from_row(Comment, row) if row else None


async def select_comments_with_author(
    db: Connection, conditions: str, params: Params
) -> list[tuple[Comment, User]]:
    rows = await fetch_all(
        db,
        f"""
        SELECT {COMMENT_COLUMNS}, {user_columns("u", "author")}
        FROM comments c JOIN users u ON u.id = c.author_id AND u.status = 'active'
        WHERE c.deleted_at IS NULL {conditions}
        """,
        params,
    )
    return [(from_row(Comment, row), from_row(User, row, "author.")) for row in rows]


async def can_comment(db: Connection, viewer_id: str, post_id: str) -> bool:
    found = await get_post_with_author(db, post_id)
    if found is None:
        return False
    post, _ = found
    viewer = await load_viewer(db, viewer_id)
    if viewer is None or not viewer.can_view(post) or post.window_ended_at is not None:
        return False
    if viewer.id == post.author_id:
        return viewer.has_posted_today
    if post.visibility == "public":
        return True
    return post.author_id in viewer.friends
