import re
from dataclasses import dataclass

from ..config import REGIONS
from ..db import Connection, fetch_one, from_row
from ..image_signature import image_signing_key, signed_image_url

USERNAME_PATTERN = re.compile(r"[a-z0-9_.-]{1,64}", re.ASCII)

USER_COLUMNS = (
    "id",
    "username",
    "secret_key_hash",
    "display_name",
    "bio",
    "profile_image_id",
    "region",
    "account_type",
    "status",
)


def valid_username(value: str) -> bool:
    return USERNAME_PATTERN.fullmatch(value) is not None


def valid_user_text(value: str, minimum: int, maximum: int) -> bool:
    return minimum <= len(value) <= maximum


def valid_region(value: str) -> bool:
    return value in REGIONS


def user_columns(alias: str = "", prefix: str = "") -> str:
    """SELECT する users の列。prefix を付けると "<prefix>.<列名>" の名前で取り出す"""
    table = f"{alias}." if alias else ""
    if prefix:
        return ", ".join(f'{table}{c} AS "{prefix}.{c}"' for c in USER_COLUMNS)
    return ", ".join(table + c for c in USER_COLUMNS)


@dataclass(slots=True)
class User:
    id: str
    username: str
    secret_key_hash: str
    display_name: str
    bio: str
    profile_image_id: str | None
    region: str
    account_type: str
    status: str

    @property
    def image_key(self) -> bytes:
        return image_signing_key(self.secret_key_hash)

    def signed_image_url(self, image_id: str) -> str:
        return signed_image_url(self.image_key, image_id)

    @property
    def profile_image_url(self) -> str | None:
        if self.profile_image_id is None:
            return None
        return self.signed_image_url(self.profile_image_id)


async def get_user(db: Connection, user_id: str) -> User | None:
    row = await fetch_one(
        db,
        f"SELECT {user_columns()} FROM users WHERE id = %(id)s AND status = 'active'",
        {"id": user_id},
    )
    return from_row(User, row) if row else None


async def lock_user(db: Connection, user_id: str, *, active_only: bool = False) -> bool:
    """ユーザーの行をロックする。行がなければ False"""
    condition = " AND status = 'active'" if active_only else ""
    row = await fetch_one(
        db,
        f"SELECT id FROM users WHERE id = %(id)s{condition} FOR NO KEY UPDATE",
        {"id": user_id},
    )
    return row is not None
