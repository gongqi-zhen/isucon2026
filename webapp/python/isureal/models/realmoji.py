from dataclasses import dataclass
from datetime import datetime

from ..db import Connection, Params, fetch_all, fetch_one, from_row
from .user import User, user_columns

REACTION_TYPES = frozenset({"like", "smile", "surprise", "heart", "laugh", "instant"})

REALMOJI_COLUMNS = "r.id, r.post_id, r.reactor_id, r.image_id, r.reaction_type, r.created_at, r.replaced_at, r.deleted_at"


@dataclass(slots=True)
class RealMoji:
    id: str
    post_id: str
    reactor_id: str
    image_id: str
    reaction_type: str
    created_at: datetime
    replaced_at: datetime | None
    deleted_at: datetime | None


async def get_realmoji(db: Connection, realmoji_id: str) -> RealMoji | None:
    row = await fetch_one(
        db,
        f"SELECT {REALMOJI_COLUMNS} FROM realmojis r WHERE r.id = %(id)s",
        {"id": realmoji_id},
    )
    return from_row(RealMoji, row) if row else None


async def select_current_realmojis(
    db: Connection, conditions: str, params: Params
) -> list[tuple[RealMoji, User]]:
    rows = await fetch_all(
        db,
        f"""
        SELECT {REALMOJI_COLUMNS}, {user_columns("u", "reactor")}
        FROM realmojis r JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
        WHERE r.replaced_at IS NULL AND r.deleted_at IS NULL {conditions}
        """,
        params,
    )
    return [(from_row(RealMoji, row), from_row(User, row, "reactor.")) for row in rows]
