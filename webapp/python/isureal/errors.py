from collections.abc import Mapping


class APIError(Exception):
    """API のエラー。{"error": code} として返す"""

    def __init__(
        self, status: int, code: str, headers: Mapping[str, str] | None = None
    ) -> None:
        super().__init__(code)
        self.status = status
        self.code = code
        self.headers = headers


class NoRowsError(Exception):
    """あるはずの行が見つからなかった"""

    def __init__(self) -> None:
        super().__init__("sql: no rows in result set")
