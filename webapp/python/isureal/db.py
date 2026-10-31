from collections.abc import AsyncIterator, Mapping
from dataclasses import fields
from typing import Annotated, Any

from fastapi import Depends, Request
from psycopg import AsyncConnection
from psycopg.rows import DictRow, dict_row
from psycopg.types.numeric import FloatLoader
from psycopg_pool import AsyncConnectionPool

from .config import Config
from .errors import NoRowsError

type Connection = AsyncConnection[DictRow]
type Pool = AsyncConnectionPool[Connection]
type Params = Mapping[str, Any] | None


async def _configure(conn: Connection) -> None:
    # latitude / longitude（numeric）を float で受け取る
    conn.adapters.register_loader("numeric", FloatLoader)


def create_pool(config: Config) -> Pool:
    return AsyncConnectionPool(
        config.database_dsn,
        connection_class=AsyncConnection[DictRow],
        kwargs={"autocommit": True, "row_factory": dict_row},
        configure=_configure,
        min_size=1,
        max_size=config.db_pool_size,
        open=False,
    )


async def get_db(request: Request) -> AsyncIterator[Connection]:
    pool: Pool = request.app.state.pool
    async with pool.connection() as conn:
        yield conn


# ハンドラーが返った時点で接続をプールに返す（通知のストリーム中は保持しない）
DB = Annotated[Connection, Depends(get_db, scope="function")]


async def fetch_one(
    db: Connection, query: str, params: Params = None
) -> DictRow | None:
    cursor = await db.execute(query, params)
    return await cursor.fetchone()


async def fetch_all(db: Connection, query: str, params: Params = None) -> list[DictRow]:
    cursor = await db.execute(query, params)
    return await cursor.fetchall()


async def fetch_value(db: Connection, query: str, params: Params = None) -> Any:
    """最初の行の最初の列を返す。行がなければ NoRowsError"""
    row = await fetch_one(db, query, params)
    if row is None:
        raise NoRowsError
    return next(iter(row.values()))


async def execute(db: Connection, query: str, params: Params = None) -> int:
    """クエリを実行し、変更した行数を返す"""
    cursor = await db.execute(query, params)
    return cursor.rowcount


def from_row[T](cls: type[T], row: Mapping[str, Any], prefix: str = "") -> T:
    """dataclass のフィールド名（prefix 付き）の列から dataclass を作る"""
    return cls(**{f.name: row[prefix + f.name] for f in fields(cls)})  # type: ignore[arg-type]
