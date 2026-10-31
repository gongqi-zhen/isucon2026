import uuid
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from dataclasses import dataclass
from datetime import datetime
from http import HTTPStatus

from ..db import Connection, execute, fetch_all, fetch_one, fetch_value, from_row
from ..errors import APIError
from .user import lock_user


@dataclass(slots=True)
class FriendRequest:
    id: str
    requester_id: str
    recipient_id: str
    status: str
    created_at: datetime
    updated_at: datetime


async def get_friend_request(db: Connection, request_id: str) -> FriendRequest | None:
    row = await fetch_one(
        db,
        """
        SELECT id, requester_id, recipient_id, status, created_at, updated_at
        FROM friend_requests WHERE id = %(id)s
        """,
        {"id": request_id},
    )
    return from_row(FriendRequest, row) if row else None


@asynccontextmanager
async def relationship_transaction(
    db: Connection, actor: str, target: str
) -> AsyncIterator[None]:
    """2 人のユーザーを ID 順にロックしたトランザクションを開始する"""
    if actor == target or not target:
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_relationship")
    async with db.transaction():
        for user_id in sorted((actor, target)):
            if not await lock_user(db, user_id, active_only=True):
                raise APIError(HTTPStatus.NOT_FOUND, "relationship_not_found")
        yield


async def is_blocked(db: Connection, actor: str, target: str) -> bool:
    blocked: bool = await fetch_value(
        db,
        """
        SELECT EXISTS(
          SELECT 1 FROM block_relations
          WHERE (blocker_id = %(actor)s AND blocked_id = %(target)s)
             OR (blocker_id = %(target)s AND blocked_id = %(actor)s)
        )
        """,
        {"actor": actor, "target": target},
    )
    return blocked


async def insert_friendship(db: Connection, request: FriendRequest) -> None:
    low, high = sorted((request.requester_id, request.recipient_id))
    await execute(
        db,
        """
        INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id)
        VALUES (%(id)s, %(low)s, %(high)s, %(request_id)s)
        """,
        {"id": str(uuid.uuid4()), "low": low, "high": high, "request_id": request.id},
    )


async def get_friend_ids(db: Connection, user_id: str) -> list[str]:
    rows = await fetch_all(
        db,
        """
        SELECT user_low_id, user_high_id FROM friendships
        WHERE user_low_id = %(user_id)s OR user_high_id = %(user_id)s
        """,
        {"user_id": user_id},
    )
    return [
        row["user_high_id"] if row["user_low_id"] == user_id else row["user_low_id"]
        for row in rows
    ]


async def get_blocked_user_ids(db: Connection, user_id: str) -> set[str]:
    rows = await fetch_all(
        db,
        """
        SELECT CASE WHEN blocker_id = %(user_id)s THEN blocked_id ELSE blocker_id END AS id
        FROM block_relations WHERE blocker_id = %(user_id)s OR blocked_id = %(user_id)s
        """,
        {"user_id": user_id},
    )
    return {row["id"] for row in rows}
