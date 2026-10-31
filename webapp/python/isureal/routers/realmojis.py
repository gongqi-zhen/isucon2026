import uuid
from http import HTTPStatus
from typing import Annotated

from fastapi import APIRouter, Depends, Query, Response

from ..auth import RegionalSession
from ..binding import Form, cursor_time, form_body
from ..db import DB, execute, fetch_one
from ..errors import APIError, NoRowsError
from ..jpeg import valid_jpeg
from ..models.post import can_view_post, get_post
from ..models.realmoji import REACTION_TYPES, get_realmoji, select_current_realmojis
from ..models.relationship import relationship_transaction
from ..models.user import get_user, lock_user
from ..schemas import Page, RealMojiResponse

router = APIRouter(prefix="/api")


def interaction_forbidden() -> APIError:
    return APIError(HTTPStatus.FORBIDDEN, "interaction_forbidden")


@router.post("/posts/{post_id}/realmojis", status_code=HTTPStatus.CREATED)
async def replace_realmoji(
    db: DB,
    session: RegionalSession,
    post_id: str,
    form: Annotated[Form, Depends(form_body)],
) -> RealMojiResponse:
    image = form.file("image")
    reaction_type = form.get("reactionType") or ""
    if image is None or reaction_type not in REACTION_TYPES:
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_realmoji")
    data = await image.read()
    if not await valid_jpeg(data, 512):
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_realmoji")
    actor = session.user_id
    post = await get_post(db, post_id)
    if post is None or actor == post.author_id:
        raise interaction_forbidden()
    async with relationship_transaction(db, actor, post.author_id):
        if not await can_view_post(db, actor, post.id):
            raise interaction_forbidden()
        await execute(
            db,
            """
            UPDATE realmojis SET replaced_at = clock_timestamp()
            WHERE post_id = %(post_id)s AND reactor_id = %(actor)s
              AND replaced_at IS NULL AND deleted_at IS NULL
            """,
            {"post_id": post.id, "actor": actor},
        )
        image_id = str(uuid.uuid4())
        await execute(
            db,
            """
            INSERT INTO image_objects (id, owner_user_id, data, media_type)
            VALUES (%(id)s, %(actor)s, %(data)s, 'image/jpeg')
            """,
            {"id": image_id, "actor": actor, "data": data},
        )
        realmoji_id = str(uuid.uuid4())
        await execute(
            db,
            """
            INSERT INTO realmojis (id, post_id, reactor_id, image_id, reaction_type, created_at)
            VALUES (%(id)s, %(post_id)s, %(actor)s, %(image_id)s, %(reaction_type)s, clock_timestamp())
            """,
            {
                "id": realmoji_id,
                "post_id": post.id,
                "actor": actor,
                "image_id": image_id,
                "reaction_type": reaction_type,
            },
        )
        reaction = await get_realmoji(db, realmoji_id)
        user = await get_user(db, actor)
        if reaction is None or user is None:
            raise NoRowsError
    return RealMojiResponse.of(reaction, user)


@router.get("/posts/{post_id}/realmojis")
async def get_realmojis(
    db: DB,
    session: RegionalSession,
    post_id: str,
    before_created_at: Annotated[str | None, Query(alias="beforeCreatedAt")] = None,
    before_id: Annotated[str, Query(alias="beforeId")] = "",
) -> Page[RealMojiResponse]:
    before_at = cursor_time(before_created_at)
    if (before_at is None) != (before_id == ""):
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_pagination")
    if not await can_view_post(db, session.user_id, post_id):
        raise interaction_forbidden()
    reactions = await select_current_realmojis(
        db,
        """
        AND r.post_id = %(post_id)s
        AND (%(before_at)s::timestamptz IS NULL OR (r.created_at, r.id) < (%(before_at)s, %(before_id)s))
        ORDER BY r.created_at DESC, r.id DESC LIMIT 51
        """,
        {"post_id": post_id, "before_at": before_at, "before_id": before_id},
    )
    return Page.of(
        [RealMojiResponse.of(reaction, reactor) for reaction, reactor in reactions]
    )


@router.delete("/realmojis/{realmoji_id}", status_code=HTTPStatus.NO_CONTENT)
async def delete_realmoji(
    db: DB, session: RegionalSession, realmoji_id: str
) -> Response:
    actor = session.user_id
    async with db.transaction():
        if not await lock_user(db, actor):
            raise NoRowsError
        record = await fetch_one(
            db,
            """
            SELECT r.reactor_id, p.author_id
            FROM realmojis r JOIN posts p ON p.id = r.post_id
            WHERE r.id = %(id)s AND r.deleted_at IS NULL FOR UPDATE OF r
            """,
            {"id": realmoji_id},
        )
        if record is None:
            raise APIError(HTTPStatus.NOT_FOUND, "interaction_not_found")
        if actor not in (record["reactor_id"], record["author_id"]):
            raise interaction_forbidden()
        await execute(
            db,
            "UPDATE realmojis SET deleted_at = clock_timestamp() WHERE id = %(id)s",
            {"id": realmoji_id},
        )
    return Response(status_code=HTTPStatus.NO_CONTENT)
