"""リクエストの解釈。Go 実装（echo の Bind）と同じ条件でエラーにする"""

import json
import re
from collections.abc import Awaitable, Callable
from datetime import datetime
from http import HTTPStatus
from typing import Any

from fastapi import Request
from pydantic import BaseModel, ConfigDict, ValidationError
from pydantic.alias_generators import to_camel
from starlette.datastructures import FormData, UploadFile
from starlette.exceptions import HTTPException
from starlette.formparsers import MultiPartException

from .errors import APIError
from .timeutil import parse_rfc3339

_INT = re.compile(r"[+-]?\d+", re.ASCII)
_FLOAT = re.compile(
    r"[+-]?(?:(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?|inf(?:inity)?|nan)",
    re.ASCII | re.IGNORECASE,
)


def http_error_message(status: HTTPStatus) -> str:
    """フレームワークが返すエラーのメッセージ（Go 実装の echo と同じ形式）"""
    return f"code={status.value}, message={status.phrase}"


def bad_request() -> APIError:
    return APIError(HTTPStatus.BAD_REQUEST, http_error_message(HTTPStatus.BAD_REQUEST))


def unsupported_media_type() -> APIError:
    status = HTTPStatus.UNSUPPORTED_MEDIA_TYPE
    return APIError(status, http_error_message(status))


def media_type(request: Request) -> str:
    return request.headers.get("content-type", "").partition(";")[0].strip()


class RequestBody(BaseModel):
    """JSON のリクエストボディ。型が違う値はエラー、知らないキーは無視する"""

    model_config = ConfigDict(alias_generator=to_camel, strict=True, frozen=True)


def _sanitize(value: Any) -> Any:
    # 不正なサロゲートは U+FFFD に置き換える（Go の encoding/json と同じ）
    if isinstance(value, str) and not value.isascii():
        return value.encode("utf-16", "surrogatepass").decode("utf-16", "replace")
    return value


def json_body[M: RequestBody](model: type[M]) -> Callable[[Request], Awaitable[M]]:
    """JSON ボディを model として受け取る依存関係を作る。ボディが空なら既定値"""

    async def dependency(request: Request) -> M:
        body = await request.body()
        if not body:
            return model()
        if media_type(request) != "application/json":
            raise unsupported_media_type()
        try:
            data = json.loads(body.decode(errors="replace"))
        except ValueError:
            raise bad_request() from None
        if data is None:
            return model()
        if not isinstance(data, dict):
            raise bad_request()
        try:
            # null は省略と同じ扱い
            return model.model_validate(
                {
                    key: _sanitize(value)
                    for key, value in data.items()
                    if value is not None
                }
            )
        except ValidationError:
            raise bad_request() from None

    return dependency


class Form:
    """multipart/form-data のボディ。同じ名前の値が複数あれば最初の値を使う"""

    def __init__(self, data: FormData | None = None) -> None:
        self._data = data or FormData()

    def get(self, name: str) -> str | None:
        values = [v for v in self._data.getlist(name) if isinstance(v, str)]
        return values[0] if values else None

    def file(self, name: str) -> UploadFile | None:
        files = [v for v in self._data.getlist(name) if isinstance(v, UploadFile)]
        return files[0] if files else None

    def float(self, name: str) -> float | None:
        value = self.get(name)
        if value is None:
            return None
        if value == "":
            return 0.0
        if not _FLOAT.fullmatch(value):
            raise bad_request()
        return float(value)


async def form_body(request: Request) -> Form:
    body = await request.body()
    if not body:
        return Form()
    match media_type(request):
        case "multipart/form-data" | "application/x-www-form-urlencoded":
            try:
                return Form(await request.form())
            except HTTPException, MultiPartException, ValueError:
                raise bad_request() from None
        case "application/json" | "application/xml" | "text/xml":
            # フォームの項目は JSON や XML からは受け取らない
            return Form()
        case _:
            raise unsupported_media_type()


def cursor_time(value: str | None) -> datetime | None:
    """ページングの時刻クエリを解釈する。送られていなければ None"""
    if value is None:
        return None
    parsed = parse_rfc3339(value)
    if parsed is None:
        raise bad_request()
    return parsed


def cursor_int(value: str | None) -> int | None:
    if value is None:
        return None
    if value == "":
        return 0
    if not _INT.fullmatch(value):
        raise bad_request()
    try:
        parsed = int(value)
    except ValueError:
        raise bad_request() from None
    if not -(2**63) <= parsed < 2**63:
        raise bad_request()
    return parsed
