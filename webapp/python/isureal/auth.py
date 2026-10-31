"""認証とリージョンの確認"""

from http import HTTPStatus
from typing import Annotated

from fastapi import Depends, Request

from .config import Config
from .db import DB
from .errors import APIError
from .models.session import AdminSession, Session, find_admin_session, find_session


def get_config(request: Request) -> Config:
    config: Config = request.app.state.config
    return config


AppConfig = Annotated[Config, Depends(get_config)]


def _bearer_token(request: Request, error_code: str) -> str | None:
    header = request.headers.get("authorization", "")
    if not header:
        return None
    if not header.startswith("Bearer "):
        raise APIError(HTTPStatus.UNAUTHORIZED, error_code)
    return header.removeprefix("Bearer ")


async def current_session(request: Request, db: DB) -> Session:
    token = _bearer_token(request, "invalid_session")
    session = await find_session(db, token) if token is not None else None
    if session is None:
        raise APIError(HTTPStatus.UNAUTHORIZED, "invalid_session")
    request.state.session = session
    return session


CurrentSession = Annotated[Session, Depends(current_session)]


async def regional_session(session: CurrentSession, config: AppConfig) -> Session:
    """このサーバーのリージョンのユーザーだけを通す"""
    if session.region != config.region:
        raise APIError(HTTPStatus.FORBIDDEN, "region_mismatch")
    return session


RegionalSession = Annotated[Session, Depends(regional_session)]


async def current_admin_session(request: Request, db: DB) -> AdminSession:
    token = _bearer_token(request, "invalid_administrator_session")
    session = await find_admin_session(db, token) if token is not None else None
    if session is None:
        raise APIError(HTTPStatus.UNAUTHORIZED, "invalid_administrator_session")
    return session


CurrentAdminSession = Annotated[AdminSession, Depends(current_admin_session)]
