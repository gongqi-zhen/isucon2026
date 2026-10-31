import unicodedata
from http import HTTPStatus
from typing import Annotated

from fastapi import APIRouter, Query

from ..auth import RegionalSession
from ..db import DB, fetch_all, from_row
from ..errors import APIError
from ..models.relationship import get_blocked_user_ids, get_friend_ids, is_blocked
from ..models.user import User, get_user, user_columns, valid_user_text
from ..schemas import Page, UserResponse

router = APIRouter(prefix="/api/users")


def normalize_search_text(value: str) -> str:
    return unicodedata.normalize("NFC", unicodedata.normalize("NFC", value).casefold())


@router.get("")
async def search_users(
    db: DB,
    session: RegionalSession,
    q: str = "",
    before_username: Annotated[str, Query(alias="beforeUsername")] = "",
    before_id: Annotated[str, Query(alias="beforeId")] = "",
) -> Page[UserResponse]:
    if not valid_user_text(q, 1, 100):
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_relationship")
    if (before_username == "") != (before_id == ""):
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_pagination")
    rows = await fetch_all(
        db,
        f"""
        SELECT {user_columns()} FROM users
        WHERE status = 'active' AND id <> %(viewer)s
          AND (%(before_username)s = '' OR (username, id) > (%(before_username)s, %(before_id)s))
        ORDER BY username, id
        """,
        {
            "viewer": session.user_id,
            "before_username": before_username,
            "before_id": before_id,
        },
    )
    blocked = await get_blocked_user_ids(db, session.user_id)
    query = normalize_search_text(q)
    items: list[UserResponse] = []
    for row in rows:
        user = from_row(User, row)
        if (
            query not in normalize_search_text(user.username)
            and query not in normalize_search_text(user.display_name)
        ) or user.id in blocked:
            continue
        items.append(
            UserResponse.of(user, show_bio=user.account_type == "certified_public")
        )
        if len(items) > 50:
            break
    return Page.of(items)


@router.get("/{user_id}")
async def get_user_profile(
    db: DB, session: RegionalSession, user_id: str
) -> UserResponse:
    user = await get_user(db, user_id)
    if user is None or await is_blocked(db, session.user_id, user.id):
        raise APIError(HTTPStatus.NOT_FOUND, "relationship_not_found")
    friends = await get_friend_ids(db, session.user_id)
    return UserResponse.of(
        user, show_bio=user.account_type == "certified_public" or user.id in friends
    )
