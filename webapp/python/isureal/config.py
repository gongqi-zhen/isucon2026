import os
from dataclasses import dataclass
from typing import Self
from urllib.parse import quote

REGIONS = frozenset({"america", "europe", "asia"})


def _env(key: str, default: str) -> str:
    return os.getenv(key) or default


@dataclass(frozen=True, slots=True)
class Config:
    region: str
    db_host: str
    db_port: str
    db_user: str
    db_password: str
    db_name: str
    db_pool_size: int
    log_level: str

    @classmethod
    def from_env(cls) -> Self:
        return cls(
            region=_env("ISUREAL_REGION", "asia"),
            db_host=_env("ISUREAL_DB_HOST", "127.0.0.1"),
            db_port=_env("ISUREAL_DB_PORT", "5432"),
            db_user=_env("ISUREAL_DB_USER", "isureal"),
            db_password=_env("ISUREAL_DB_PASSWORD", "isureal"),
            db_name=_env("ISUREAL_DB_NAME", "isureal"),
            db_pool_size=int(_env("ISUREAL_DB_POOL_SIZE", "7")),
            log_level=_env("ISUREAL_LOG_LEVEL", "debug"),
        )

    @property
    def database_dsn(self) -> str:
        host = f"[{self.db_host}]" if ":" in self.db_host else self.db_host
        user, password = quote(self.db_user, safe=""), quote(self.db_password, safe="")
        return f"postgresql://{user}:{password}@{host}:{self.db_port}/{quote(self.db_name, safe='')}"
