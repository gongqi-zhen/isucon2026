import hashlib
import uuid
from dataclasses import dataclass

from ..db import Connection, execute, fetch_one
from ..errors import NoRowsError
from .user import lock_user


def hash_token(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()


@dataclass(slots=True)
class Session:
    user_id: str
    token_hash: str
    region: str


@dataclass(slots=True)
class AdminSession:
    administrator_id: str
    token_hash: str


async def find_session(db: Connection, token: str) -> Session | None:
    row = await fetch_one(
        db,
        """
        SELECT s.user_id, s.token_hash, u.region
        FROM login_sessions s JOIN users u ON u.id = s.user_id
        WHERE s.token_hash = %(token_hash)s
          AND s.active = true AND u.status = 'active'
        """,
        {"token_hash": hash_token(token)},
    )
    return Session(**row) if row else None


async def create_login_session(db: Connection, user_id: str) -> str:
    """ユーザーのセッションを作り直してトークンを返す。トランザクション内で呼ぶこと"""
    token = str(uuid.uuid4())
    if not await lock_user(db, user_id, active_only=True):
        raise NoRowsError
    await execute(
        db,
        """
        UPDATE login_sessions
        SET active = false, invalidated_at = clock_timestamp(), reason = 'replaced'
        WHERE user_id = %(user_id)s AND active = true
        """,
        {"user_id": user_id},
    )
    await execute(
        db,
        "INSERT INTO login_sessions (id, user_id, token_hash) VALUES (%(id)s, %(user_id)s, %(token_hash)s)",
        {"id": str(uuid.uuid4()), "user_id": user_id, "token_hash": hash_token(token)},
    )
    return token


async def invalidate_login_session(db: Connection, session: Session) -> None:
    async with db.transaction():
        if not await lock_user(db, session.user_id):
            raise NoRowsError
        await execute(
            db,
            """
            UPDATE login_sessions
            SET active = false, invalidated_at = clock_timestamp(), reason = 'logout'
            WHERE user_id = %(user_id)s AND active = true AND token_hash = %(token_hash)s
            """,
            {"user_id": session.user_id, "token_hash": session.token_hash},
        )


async def find_admin_session(db: Connection, token: str) -> AdminSession | None:
    row = await fetch_one(
        db,
        """
        SELECT administrator_id, token_hash
        FROM administrator_sessions
        WHERE token_hash = %(token_hash)s AND active = true
        """,
        {"token_hash": hash_token(token)},
    )
    return AdminSession(**row) if row else None


async def create_admin_session(db: Connection, administrator_id: str) -> str:
    token = str(uuid.uuid4())
    await execute(
        db,
        """
        INSERT INTO administrator_sessions (id, administrator_id, token_hash)
        VALUES (%(id)s, %(administrator_id)s, %(token_hash)s)
        """,
        {
            "id": str(uuid.uuid4()),
            "administrator_id": administrator_id,
            "token_hash": hash_token(token),
        },
    )
    return token


async def invalidate_admin_session(db: Connection, session: AdminSession) -> None:
    await execute(
        db,
        """
        UPDATE administrator_sessions
        SET active = false, invalidated_at = clock_timestamp()
        WHERE administrator_id = %(administrator_id)s AND token_hash = %(token_hash)s AND active = true
        """,
        {
            "administrator_id": session.administrator_id,
            "token_hash": session.token_hash,
        },
    )
