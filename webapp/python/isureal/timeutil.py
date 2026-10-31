import re
from datetime import UTC, date, datetime

_RFC3339 = re.compile(
    r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})", re.ASCII
)
_DATE = re.compile(r"\d{4}-\d{2}-\d{2}", re.ASCII)
_MONTH = re.compile(r"\d{4}-\d{2}", re.ASCII)


def now() -> datetime:
    return datetime.now(UTC)


def format_rfc3339(value: datetime) -> str:
    """ローカルタイムゾーンの RFC 3339 にする（Go の time.RFC3339Nano と同じ）。
    小数秒は末尾の 0 を省き、UTC なら Z を付ける"""
    value = value.astimezone()
    text = value.strftime("%Y-%m-%dT%H:%M:%S")
    if value.microsecond:
        text += f".{value.microsecond:06d}".rstrip("0")
    offset = value.strftime("%z")
    if offset == "+0000":
        return text + "Z"
    return f"{text}{offset[:3]}:{offset[3:]}"


def parse_rfc3339(value: str) -> datetime | None:
    if not _RFC3339.fullmatch(value):
        return None
    try:
        return datetime.fromisoformat(value)
    except ValueError:
        return None


def parse_date(value: str) -> date | None:
    """YYYY-MM-DD を解釈する"""
    if not _DATE.fullmatch(value):
        return None
    try:
        return date.fromisoformat(value)
    except ValueError:
        return None


def parse_month(value: str) -> date | None:
    """YYYY-MM を解釈し、その月の 1 日を返す"""
    if not _MONTH.fullmatch(value):
        return None
    return parse_date(value + "-01")


def add_months(value: date, months: int) -> date:
    month = value.month - 1 + months
    return value.replace(year=value.year + month // 12, month=month % 12 + 1)
