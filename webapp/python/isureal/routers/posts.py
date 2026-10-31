import uuid
from http import HTTPStatus
from typing import Annotated

from fastapi import APIRouter, Depends, Response
from psycopg.rows import DictRow

from ..auth import RegionalSession
from ..binding import Form, RequestBody, form_body, json_body
from ..db import DB, Connection, execute, fetch_one, fetch_value, from_row
from ..errors import APIError, NoRowsError
from ..jpeg import valid_jpeg
from ..models.post import (
    VISIBILITIES,
    Location,
    get_post,
    get_post_with_author,
    load_viewer,
)
from ..models.posting_window import PostingWindow, get_posting_window
from ..models.user import get_user, lock_user, valid_user_text
from ..schemas import CurrentPostingWindowResponse, PostingWindowResponse, PostResponse
from ..timeutil import now

router = APIRouter(prefix="/api")


def post_not_found() -> APIError:
    return APIError(HTTPStatus.NOT_FOUND, "post_not_found")


def post_not_editable() -> APIError:
    return APIError(HTTPStatus.FORBIDDEN, "post_not_editable")


def post_slot_unavailable() -> APIError:
    return APIError(HTTPStatus.CONFLICT, "post_slot_unavailable")


async def lock_current_window(db: Connection, region: str) -> DictRow | None:
    return await fetch_one(
        db,
        """
        SELECT id, local_date, starts_at FROM posting_windows
        WHERE region = %(region)s AND ended_at IS NULL FOR UPDATE
        """,
        {"region": region},
    )


@router.post("/posts", status_code=HTTPStatus.CREATED)
async def create_post(
    db: DB, session: RegionalSession, form: Annotated[Form, Depends(form_body)]
) -> PostResponse:
    image = form.file("image")
    caption = form.get("caption") or ""
    visibility = form.get("visibility") or ""
    location = Location(
        kind=form.get("location_kind") or "",
        latitude=form.float("latitude"),
        longitude=form.float("longitude"),
        locality=form.get("locality"),
    )
    if (
        image is None
        or not valid_user_text(caption, 0, 1000)
        or visibility not in VISIBILITIES
        or not location.is_valid()
    ):
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_post")
    image_data = await image.read()
    if not await valid_jpeg(image_data, 4096):
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_post")

    user_id = session.user_id
    async with db.transaction():
        if not await lock_user(db, user_id):
            raise NoRowsError
        user = await get_user(db, user_id)
        if user is None:
            raise NoRowsError
        window = await lock_current_window(db, user.region)
        if window is None:
            # 窓の切り替えを待った直後は新しい窓が見えないので、もう一度探す
            window = await lock_current_window(db, user.region)
        if window is None:
            raise post_slot_unavailable()
        counts = await fetch_one(
            db,
            """
            SELECT count(*) FILTER (WHERE deleted_at IS NULL) AS active,
                   count(*) FILTER (WHERE deleted_at IS NOT NULL) AS deleted
            FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
            WHERE p.author_id = %(user_id)s AND w.local_date = %(local_date)s
            """,
            {"user_id": user_id, "local_date": window["local_date"]},
        )
        if counts is None:
            raise NoRowsError
        posted_at = now()
        if (
            counts["active"] != 0
            or counts["deleted"] >= 2
            or posted_at < window["starts_at"]
        ):
            raise post_slot_unavailable()
        image_id = str(uuid.uuid4())
        await execute(
            db,
            """
            INSERT INTO image_objects (id, owner_user_id, data, media_type, created_at)
            VALUES (%(id)s, %(user_id)s, %(data)s, 'image/jpeg', %(created_at)s)
            """,
            {
                "id": image_id,
                "user_id": user_id,
                "data": image_data,
                "created_at": posted_at,
            },
        )
        post_id = str(uuid.uuid4())
        await execute(
            db,
            """
            INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility,
                               location_kind, latitude, longitude, locality, posted_at)
            VALUES (%(id)s, %(author_id)s, %(window_id)s, %(image_id)s, %(caption)s, %(visibility)s,
                    %(location_kind)s, %(latitude)s, %(longitude)s, %(locality)s, %(posted_at)s)
            """,
            {
                "id": post_id,
                "author_id": user_id,
                "window_id": window["id"],
                "image_id": image_id,
                "caption": caption,
                "visibility": visibility,
                "location_kind": location.kind,
                "latitude": location.latitude,
                "longitude": location.longitude,
                "locality": location.locality,
                "posted_at": posted_at,
            },
        )
        post = await get_post(db, post_id)
        if post is None:
            raise NoRowsError
    return PostResponse.of(post, user)


@router.get("/posting-window/current")
async def get_current_posting_window(
    db: DB, session: RegionalSession
) -> CurrentPostingWindowResponse:
    row = await fetch_one(
        db,
        """
        SELECT w.id, w.region, w.local_date, w.starts_at, w.on_time_deadline, w.ended_at,
               count(p.id) FILTER (WHERE p.deleted_at IS NULL) AS active,
               count(p.id) FILTER (WHERE p.deleted_at IS NOT NULL) AS deleted
        FROM users u JOIN posting_windows w ON w.region = u.region AND w.ended_at IS NULL
        LEFT JOIN posting_windows posted_window ON posted_window.local_date = w.local_date
        LEFT JOIN posts p ON p.author_id = u.id AND p.posting_window_id = posted_window.id
        WHERE u.id = %(user_id)s
        GROUP BY w.id
        """,
        {"user_id": session.user_id},
    )
    if row is None:
        return CurrentPostingWindowResponse()
    window = from_row(PostingWindow, row)
    return CurrentPostingWindowResponse(
        window=PostingWindowResponse.of(window),
        has_posted=row["active"] > 0,
        can_post=row["active"] == 0
        and row["deleted"] < 2
        and now() >= window.starts_at,
        deletion_count=row["deleted"],
    )


@router.get("/posts/me/current")
async def get_my_current_post(db: DB, session: RegionalSession) -> PostResponse:
    row = await fetch_one(
        db,
        """
        SELECT p.id FROM posts p
        JOIN posting_windows w ON w.id = p.posting_window_id
        JOIN users u ON u.id = p.author_id
        JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                                    AND current.local_date = w.local_date
        WHERE p.author_id = %(user_id)s AND p.deleted_at IS NULL
        """,
        {"user_id": session.user_id},
    )
    post = await get_post(db, row["id"]) if row else None
    if post is None:
        raise post_not_found()
    user = await get_user(db, session.user_id)
    if user is None:
        raise NoRowsError
    return PostResponse.of(post, user)


@router.get("/posts/{post_id}")
async def get_post_detail(
    db: DB, session: RegionalSession, post_id: str
) -> PostResponse:
    found = await get_post_with_author(db, post_id)
    if found is None:
        raise post_not_found()
    post, author = found
    viewer = await load_viewer(db, session.user_id)
    if viewer is None or not viewer.can_view(post):
        raise post_not_found()
    return PostResponse.of(post, author)


class UpdatePostRequest(RequestBody):
    caption: str | None = None
    visibility: str | None = None


@router.patch("/posts/{post_id}")
async def update_post(
    db: DB,
    session: RegionalSession,
    post_id: str,
    body: Annotated[UpdatePostRequest, Depends(json_body(UpdatePostRequest))],
) -> PostResponse:
    if (
        (body.caption is None and body.visibility is None)
        or (body.caption is not None and not valid_user_text(body.caption, 0, 1000))
        or (body.visibility is not None and body.visibility not in VISIBILITIES)
    ):
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_post")
    user_id = session.user_id
    async with db.transaction():
        if not await lock_user(db, user_id):
            raise NoRowsError
        post = await get_post(db, post_id)
        if post is None:
            raise post_not_found()
        if post.author_id != user_id:
            raise post_not_editable()
        window = await get_posting_window(db, post.posting_window_id)
        if window is None:
            raise NoRowsError
        if window.ended_at is not None:
            raise post_not_editable()
        for column, value in (
            ("caption", body.caption),
            ("visibility", body.visibility),
        ):
            if value is not None:
                await execute(
                    db,
                    f"UPDATE posts SET {column} = %(value)s WHERE id = %(id)s",
                    {"id": post.id, "value": value},
                )
        post = await get_post(db, post.id)
        user = await get_user(db, user_id)
        if post is None or user is None:
            raise NoRowsError
    return PostResponse.of(post, user)


@router.delete("/posts/{post_id}", status_code=HTTPStatus.NO_CONTENT)
async def delete_post(db: DB, session: RegionalSession, post_id: str) -> Response:
    user_id = session.user_id
    async with db.transaction():
        if not await lock_user(db, user_id):
            raise NoRowsError
        post = await get_post(db, post_id)
        if post is None:
            raise post_not_found()
        if post.author_id != user_id:
            raise post_not_editable()
        window = await get_posting_window(db, post.posting_window_id)
        if window is None:
            raise NoRowsError
        deletions = await fetch_value(
            db,
            """
            SELECT count(*) FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
            WHERE p.author_id = %(user_id)s AND w.local_date = %(local_date)s AND p.deleted_at IS NOT NULL
            """,
            {"user_id": user_id, "local_date": window.local_date},
        )
        if deletions >= 2:
            raise post_slot_unavailable()
        await execute(
            db,
            "UPDATE posts SET deleted_at = clock_timestamp() WHERE id = %(id)s",
            {"id": post.id},
        )
    return Response(status_code=HTTPStatus.NO_CONTENT)
