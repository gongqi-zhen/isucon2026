import uuid
from http import HTTPStatus
from typing import Annotated

from fastapi import APIRouter, Depends, Query, Response

from ..auth import RegionalSession
from ..binding import RequestBody, cursor_time, json_body
from ..db import DB, execute, fetch_one
from ..errors import APIError, NoRowsError
from ..models.comment import can_comment, get_comment, select_comments_with_author
from ..models.post import get_post
from ..models.user import get_user, lock_user, valid_user_text
from ..schemas import CommentResponse, Page

router = APIRouter(prefix="/api")


def interaction_forbidden() -> APIError:
    return APIError(HTTPStatus.FORBIDDEN, "interaction_forbidden")


class CreateCommentRequest(RequestBody):
    body: str = ""


@router.post("/posts/{post_id}/comments", status_code=HTTPStatus.CREATED)
async def create_comment(
    db: DB,
    session: RegionalSession,
    post_id: str,
    body: Annotated[CreateCommentRequest, Depends(json_body(CreateCommentRequest))],
) -> CommentResponse:
    if not valid_user_text(body.body, 1, 1000):
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_interaction")
    actor = session.user_id
    post = await get_post(db, post_id)
    if post is None:
        raise interaction_forbidden()
    async with db.transaction():
        for user_id in sorted({actor, post.author_id}):
            if not await lock_user(db, user_id):
                raise NoRowsError
        if not await can_comment(db, actor, post.id):
            raise interaction_forbidden()
        comment_id = str(uuid.uuid4())
        await execute(
            db,
            """
            INSERT INTO comments (id, post_id, author_id, body, created_at)
            VALUES (%(id)s, %(post_id)s, %(author_id)s, %(body)s, clock_timestamp())
            """,
            {
                "id": comment_id,
                "post_id": post.id,
                "author_id": actor,
                "body": body.body,
            },
        )
        comment = await get_comment(db, comment_id)
        user = await get_user(db, actor)
        if comment is None or user is None:
            raise NoRowsError
    return CommentResponse.of(comment, user)


@router.get("/posts/{post_id}/comments")
async def get_comments(
    db: DB,
    session: RegionalSession,
    post_id: str,
    after_created_at: Annotated[str | None, Query(alias="afterCreatedAt")] = None,
    after_id: Annotated[str, Query(alias="afterId")] = "",
) -> Page[CommentResponse]:
    after_at = cursor_time(after_created_at)
    if (after_at is None) != (after_id == ""):
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_pagination")
    if not await can_comment(db, session.user_id, post_id):
        raise interaction_forbidden()
    comments = await select_comments_with_author(
        db,
        """
        AND c.post_id = %(post_id)s
        AND (%(after_at)s::timestamptz IS NULL OR (c.created_at, c.id) > (%(after_at)s, %(after_id)s))
        ORDER BY c.created_at, c.id LIMIT 51
        """,
        {"post_id": post_id, "after_at": after_at, "after_id": after_id},
    )
    return Page.of(
        [CommentResponse.of(comment, author) for comment, author in comments]
    )


@router.delete("/comments/{comment_id}", status_code=HTTPStatus.NO_CONTENT)
async def delete_comment(db: DB, session: RegionalSession, comment_id: str) -> Response:
    actor = session.user_id
    async with db.transaction():
        if not await lock_user(db, actor):
            raise NoRowsError
        record = await fetch_one(
            db,
            """
            SELECT c.author_id, p.author_id AS post_author_id
            FROM comments c JOIN posts p ON p.id = c.post_id
            WHERE c.id = %(id)s AND c.deleted_at IS NULL FOR UPDATE OF c
            """,
            {"id": comment_id},
        )
        if record is None:
            raise APIError(HTTPStatus.NOT_FOUND, "interaction_not_found")
        if actor not in (record["author_id"], record["post_author_id"]):
            raise interaction_forbidden()
        await execute(
            db,
            "UPDATE comments SET deleted_at = clock_timestamp() WHERE id = %(id)s",
            {"id": comment_id},
        )
    return Response(status_code=HTTPStatus.NO_CONTENT)
