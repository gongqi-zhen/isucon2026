import hashlib
import uuid
from http import HTTPStatus
from typing import Annotated

from fastapi import APIRouter, Depends, Request, Response

from ..auth import CurrentSession
from ..binding import RequestBody, json_body
from ..db import DB, execute, fetch_one, fetch_value, from_row
from ..errors import APIError, NoRowsError
from ..models.session import create_login_session, invalidate_login_session
from ..models.user import (
    User,
    get_user,
    user_columns,
    valid_region,
    valid_user_text,
    valid_username,
)
from ..schemas import LoginResponse, RegisterResponse, SessionUserResponse

router = APIRouter(prefix="/api/auth")


class RegisterRequest(RequestBody):
    username: str = ""
    display_name: str = ""
    region: str = ""


class LoginRequest(RequestBody):
    username: str = ""
    secret_key: str = ""


@router.post("/register", status_code=HTTPStatus.CREATED)
async def register(
    db: DB,
    body: Annotated[RegisterRequest, Depends(json_body(RegisterRequest))],
    request: Request,
) -> RegisterResponse:
    request.state.username = body.username
    if not (
        valid_username(body.username)
        and valid_user_text(body.display_name, 1, 100)
        and valid_region(body.region)
    ):
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_registration")
    secret_key = str(uuid.uuid4())
    async with db.transaction():
        # ユーザー名が一意である必要があります。
        await execute(db, "LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE")
        if await fetch_value(
            db,
            "SELECT EXISTS(SELECT 1 FROM users WHERE username = %(username)s)",
            {"username": body.username},
        ):
            raise APIError(HTTPStatus.CONFLICT, "username_taken")
        user_id = str(uuid.uuid4())
        await execute(
            db,
            """
            INSERT INTO users (id, username, secret_key_hash, display_name, region)
            VALUES (%(id)s, %(username)s, %(secret_key_hash)s, %(display_name)s, %(region)s)
            """,
            {
                "id": user_id,
                "username": body.username,
                "secret_key_hash": hashlib.sha256(secret_key.encode()).hexdigest(),
                "display_name": body.display_name,
                "region": body.region,
            },
        )
        user = await get_user(db, user_id)
        if user is None:
            raise NoRowsError
    return RegisterResponse(secret_key=secret_key, user=SessionUserResponse.of(user))


@router.post("/login")
async def login(
    db: DB,
    body: Annotated[LoginRequest, Depends(json_body(LoginRequest))],
    request: Request,
) -> LoginResponse:
    request.state.username = body.username
    if not valid_username(body.username):
        raise APIError(HTTPStatus.UNAUTHORIZED, "invalid_credentials")
    async with db.transaction():
        row = await fetch_one(
            db,
            f"SELECT {user_columns()} FROM users WHERE username = %(username)s FOR NO KEY UPDATE",
            {"username": body.username},
        )
        if row is None:
            raise APIError(HTTPStatus.UNAUTHORIZED, "invalid_credentials")
        user = from_row(User, row)
        secret_key_hash = hashlib.sha256(body.secret_key.encode()).hexdigest()
        if user.status != "active" or user.secret_key_hash != secret_key_hash:
            raise APIError(HTTPStatus.UNAUTHORIZED, "invalid_credentials")
        token = await create_login_session(db, user.id)
    return LoginResponse(token=token, user=SessionUserResponse.of(user))


@router.post("/logout", status_code=HTTPStatus.NO_CONTENT)
async def logout(db: DB, session: CurrentSession) -> Response:
    await invalidate_login_session(db, session)
    return Response(status_code=HTTPStatus.NO_CONTENT)
