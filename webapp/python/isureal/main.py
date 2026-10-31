import logging
from collections.abc import AsyncIterator, Mapping
from contextlib import asynccontextmanager
from http import HTTPStatus

from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse
from starlette.exceptions import HTTPException

from .action_log import ActionLogMiddleware
from .binding import http_error_message
from .config import Config
from .db import create_pool
from .errors import APIError
from .ip_ban import IPBanMiddleware
from .models.user import valid_region
from .routers import (
    admin,
    auth,
    comments,
    feeds,
    images,
    initialize,
    me,
    memories,
    notifications,
    posts,
    realmojis,
    relationships,
    users,
)

logger = logging.getLogger(__name__)


def error_response(
    status: int, message: str, headers: Mapping[str, str] | None = None
) -> JSONResponse:
    return JSONResponse({"error": message}, status_code=status, headers=headers)


async def api_error_handler(_: Request, exc: Exception) -> JSONResponse:
    assert isinstance(exc, APIError)
    return error_response(exc.status, exc.code, exc.headers)


async def http_exception_handler(_: Request, exc: Exception) -> JSONResponse:
    assert isinstance(exc, HTTPException)
    return error_response(
        exc.status_code, HTTPStatus(exc.status_code).phrase, exc.headers
    )


async def validation_error_handler(_: Request, exc: Exception) -> JSONResponse:
    status = HTTPStatus.BAD_REQUEST
    return error_response(status, http_error_message(status))


async def unexpected_error_handler(_: Request, exc: Exception) -> JSONResponse:
    logger.error("request failed", exc_info=exc)
    return error_response(HTTPStatus.INTERNAL_SERVER_ERROR, str(exc))


def create_app(config: Config | None = None) -> FastAPI:
    config = config or Config.from_env()
    if not valid_region(config.region):
        raise ValueError(f"invalid server region: {config.region}")

    @asynccontextmanager
    async def lifespan(app: FastAPI) -> AsyncIterator[None]:
        async with create_pool(config) as pool:
            await pool.check()
            app.state.config = config
            app.state.pool = pool
            yield

    app = FastAPI(lifespan=lifespan, redirect_slashes=False, openapi_url=None)
    app.add_exception_handler(APIError, api_error_handler)
    app.add_exception_handler(HTTPException, http_exception_handler)
    app.add_exception_handler(RequestValidationError, validation_error_handler)
    app.add_exception_handler(Exception, unexpected_error_handler)
    app.add_middleware(IPBanMiddleware)
    app.add_middleware(ActionLogMiddleware, min_level=config.log_level)
    for module in (
        initialize,
        auth,
        users,
        me,
        posts,
        feeds,
        memories,
        comments,
        realmojis,
        images,
        notifications,
        relationships,
        admin,
    ):
        app.include_router(module.router)
    return app
