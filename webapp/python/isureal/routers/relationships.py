import uuid
from http import HTTPStatus
from typing import Annotated

from fastapi import APIRouter, Depends, Query, Response

from ..auth import RegionalSession
from ..binding import RequestBody, cursor_time, json_body
from ..db import DB, execute, fetch_all, fetch_one, fetch_value, from_row
from ..errors import APIError, NoRowsError
from ..models.relationship import (
    get_friend_ids,
    get_friend_request,
    insert_friendship,
    is_blocked,
    relationship_transaction,
)
from ..models.user import User, get_user, user_columns
from ..schemas import FriendRequestResponse, Page, RelationshipResponse, UserResponse

router = APIRouter(prefix="/api")


def relationship_conflict() -> APIError:
    return APIError(HTTPStatus.CONFLICT, "relationship_conflict")


def relationship_action_forbidden() -> APIError:
    return APIError(HTTPStatus.FORBIDDEN, "relationship_action_forbidden")


def no_content() -> Response:
    return Response(status_code=HTTPStatus.NO_CONTENT)


class CreateFriendRequestRequest(RequestBody):
    recipient_id: str = ""


@router.post("/friend-requests", status_code=HTTPStatus.CREATED)
async def create_friend_request(
    db: DB,
    session: RegionalSession,
    body: Annotated[
        CreateFriendRequestRequest, Depends(json_body(CreateFriendRequestRequest))
    ],
) -> FriendRequestResponse:
    actor, recipient = session.user_id, body.recipient_id
    async with relationship_transaction(db, actor, recipient):
        if await is_blocked(db, actor, recipient):
            raise relationship_conflict()
        if recipient in await get_friend_ids(db, actor):
            raise relationship_conflict()
        existing = await fetch_one(
            db,
            """
            SELECT id FROM friend_requests WHERE status = 'pending' AND
              ((requester_id = %(actor)s AND recipient_id = %(recipient)s)
               OR (requester_id = %(recipient)s AND recipient_id = %(actor)s))
            """,
            {"actor": actor, "recipient": recipient},
        )
        if existing is not None:
            # 相手からの申請があれば承認する
            request = await get_friend_request(db, existing["id"])
            if request is None:
                raise NoRowsError
            if request.requester_id == actor:
                raise relationship_conflict()
            await execute(
                db,
                "UPDATE friend_requests SET status = 'accepted', updated_at = clock_timestamp() WHERE id = %(id)s",
                {"id": request.id},
            )
            await insert_friendship(db, request)
            request_id = request.id
        else:
            request_id = str(uuid.uuid4())
            await execute(
                db,
                """
                INSERT INTO friend_requests (id, requester_id, recipient_id, status, created_at, updated_at)
                VALUES (%(id)s, %(actor)s, %(recipient)s, 'pending', clock_timestamp(), clock_timestamp())
                """,
                {"id": request_id, "actor": actor, "recipient": recipient},
            )
        request = await get_friend_request(db, request_id)
        if request is None:
            raise NoRowsError
    return FriendRequestResponse(
        relation_id=request.id, status=request.status, updated_at=request.updated_at
    )


FRIEND_REQUEST_ACTIONS = {
    "accept": "accepted",
    "reject": "rejected",
    "cancel": "cancelled",
}


@router.post("/friend-requests/{request_id}/{action}")
async def transition_friend_request(
    db: DB, session: RegionalSession, request_id: str, action: str
) -> FriendRequestResponse:
    status = FRIEND_REQUEST_ACTIONS.get(action)
    if status is None:
        raise relationship_action_forbidden()
    actor = session.user_id
    request = await get_friend_request(db, request_id)
    if request is None:
        raise APIError(HTTPStatus.NOT_FOUND, "relationship_not_found")
    allowed_actor = (
        request.requester_id if status == "cancelled" else request.recipient_id
    )
    if actor != allowed_actor:
        raise relationship_action_forbidden()
    async with relationship_transaction(db, request.requester_id, request.recipient_id):
        request = await get_friend_request(db, request_id)
        if request is None:
            raise NoRowsError
        if request.status != "pending":
            raise relationship_action_forbidden()
        if status == "accepted":
            if await is_blocked(db, request.requester_id, request.recipient_id):
                raise relationship_conflict()
            await insert_friendship(db, request)
        await execute(
            db,
            "UPDATE friend_requests SET status = %(status)s, updated_at = clock_timestamp() WHERE id = %(id)s",
            {"id": request.id, "status": status},
        )
        request = await get_friend_request(db, request.id)
        if request is None:
            raise NoRowsError
    return FriendRequestResponse(
        relation_id=request.id, status=request.status, updated_at=request.updated_at
    )


RELATIONSHIP_QUERIES = {
    "friends": """
        SELECT id, user_low_id AS first_id, user_high_id AS second_id, updated_at FROM friendships
        WHERE user_low_id = %(actor)s OR user_high_id = %(actor)s
    """,
    "following": """
        SELECT id, follower_id AS first_id, followee_id AS second_id, updated_at FROM follows
        WHERE follower_id = %(actor)s
    """,
    "followers": """
        SELECT id, follower_id AS first_id, followee_id AS second_id, updated_at FROM follows
        WHERE followee_id = %(actor)s
    """,
    "incoming-requests": """
        SELECT id, requester_id AS first_id, recipient_id AS second_id, updated_at FROM friend_requests
        WHERE recipient_id = %(actor)s AND status = 'pending'
    """,
    "outgoing-requests": """
        SELECT id, requester_id AS first_id, recipient_id AS second_id, updated_at FROM friend_requests
        WHERE requester_id = %(actor)s AND status = 'pending'
    """,
}


@router.get("/relationships/{kind}")
async def get_relationships(
    db: DB,
    session: RegionalSession,
    kind: str,
    before_updated_at: Annotated[str | None, Query(alias="beforeUpdatedAt")] = None,
    before_id: Annotated[str, Query(alias="beforeId")] = "",
) -> Page[RelationshipResponse]:
    before_at = cursor_time(before_updated_at)
    if (before_at is None) != (before_id == ""):
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_pagination")
    query = RELATIONSHIP_QUERIES.get(kind)
    if query is None:
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_relationship")
    rows = await fetch_all(
        db,
        f"""
        SELECT relations.id, relations.updated_at, {user_columns("u", "user")}
        FROM ({query}) relations
        JOIN users u ON u.status = 'active'
                    AND u.id = CASE WHEN relations.first_id = %(actor)s
                                    THEN relations.second_id ELSE relations.first_id END
        WHERE (%(before_at)s::timestamptz IS NULL
               OR (relations.updated_at, relations.id) < (%(before_at)s, %(before_id)s))
        ORDER BY relations.updated_at DESC, relations.id DESC LIMIT 51
        """,
        {"actor": session.user_id, "before_at": before_at, "before_id": before_id},
    )
    status = "pending" if kind.endswith("-requests") else kind
    items: list[RelationshipResponse] = []
    for row in rows:
        user = from_row(User, row, "user.")
        show_bio = user.account_type == "certified_public" or kind == "friends"
        items.append(
            RelationshipResponse(
                relation_id=row["id"],
                user=UserResponse.of(user, show_bio=show_bio),
                status=status,
                updated_at=row["updated_at"],
            )
        )
    return Page.of(items)


@router.delete("/friendships/{user_id}", status_code=HTTPStatus.NO_CONTENT)
async def delete_friendship(db: DB, session: RegionalSession, user_id: str) -> Response:
    actor = session.user_id
    async with relationship_transaction(db, actor, user_id):
        low, high = sorted((actor, user_id))
        if not await execute(
            db,
            "DELETE FROM friendships WHERE user_low_id = %(low)s AND user_high_id = %(high)s",
            {"low": low, "high": high},
        ):
            raise relationship_conflict()
    return no_content()


@router.put("/follows/{user_id}", status_code=HTTPStatus.NO_CONTENT)
async def follow(db: DB, session: RegionalSession, user_id: str) -> Response:
    actor = session.user_id
    async with relationship_transaction(db, actor, user_id):
        target = await get_user(db, user_id)
        if target is None:
            raise NoRowsError
        if target.account_type != "certified_public" or await is_blocked(
            db, actor, target.id
        ):
            raise relationship_conflict()
        if await fetch_value(
            db,
            "SELECT EXISTS(SELECT 1 FROM follows WHERE follower_id = %(actor)s AND followee_id = %(target)s)",
            {"actor": actor, "target": target.id},
        ):
            raise relationship_conflict()
        await execute(
            db,
            "INSERT INTO follows (id, follower_id, followee_id) VALUES (%(id)s, %(actor)s, %(target)s)",
            {"id": str(uuid.uuid4()), "actor": actor, "target": target.id},
        )
    return no_content()


@router.delete("/follows/{user_id}", status_code=HTTPStatus.NO_CONTENT)
async def unfollow(db: DB, session: RegionalSession, user_id: str) -> Response:
    actor = session.user_id
    async with relationship_transaction(db, actor, user_id):
        if not await execute(
            db,
            "DELETE FROM follows WHERE follower_id = %(actor)s AND followee_id = %(target)s",
            {"actor": actor, "target": user_id},
        ):
            raise relationship_conflict()
    return no_content()


@router.put("/blocks/{user_id}", status_code=HTTPStatus.NO_CONTENT)
async def block(db: DB, session: RegionalSession, user_id: str) -> Response:
    actor = session.user_id
    params = {"actor": actor, "target": user_id}
    async with relationship_transaction(db, actor, user_id):
        if await fetch_value(
            db,
            "SELECT EXISTS(SELECT 1 FROM block_relations WHERE blocker_id = %(actor)s AND blocked_id = %(target)s)",
            params,
        ):
            raise relationship_conflict()
        await execute(
            db,
            "INSERT INTO block_relations (id, blocker_id, blocked_id) VALUES (%(id)s, %(actor)s, %(target)s)",
            {"id": str(uuid.uuid4()), **params},
        )
        low, high = sorted((actor, user_id))
        await execute(
            db,
            "DELETE FROM friendships WHERE user_low_id = %(low)s AND user_high_id = %(high)s",
            {"low": low, "high": high},
        )
        await execute(
            db,
            """
            DELETE FROM follows
            WHERE (follower_id = %(actor)s AND followee_id = %(target)s)
               OR (follower_id = %(target)s AND followee_id = %(actor)s)
            """,
            params,
        )
        await execute(
            db,
            """
            UPDATE friend_requests SET status = 'cancelled', updated_at = clock_timestamp()
            WHERE status = 'pending' AND ((requester_id = %(actor)s AND recipient_id = %(target)s)
                                          OR (requester_id = %(target)s AND recipient_id = %(actor)s))
            """,
            params,
        )
    return no_content()


@router.delete("/blocks/{user_id}", status_code=HTTPStatus.NO_CONTENT)
async def unblock(db: DB, session: RegionalSession, user_id: str) -> Response:
    actor = session.user_id
    async with relationship_transaction(db, actor, user_id):
        if not await execute(
            db,
            "DELETE FROM block_relations WHERE blocker_id = %(actor)s AND blocked_id = %(target)s",
            {"actor": actor, "target": user_id},
        ):
            raise relationship_action_forbidden()
    return no_content()
