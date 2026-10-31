import json
import re
import sys
from http import HTTPStatus
from typing import Any, TextIO

from starlette.types import ASGIApp, Message, Receive, Scope, Send

from .ip_ban import client_ip
from .models.session import Session
from .models.user import valid_username

ACTION_NAMES = {
    "POST /api/initialize": "initialize",
    "POST /api/auth/register": "register",
    "POST /api/auth/login": "login",
    "POST /api/auth/logout": "logout",
    "GET /api/users": "search_users",
    "GET /api/users/:userId": "get_user",
    "GET /api/me/export": "export_posts",
    "GET /api/me": "get_me",
    "PATCH /api/me": "update_me",
    "PUT /api/me/profile-image": "put_profile_image",
    "DELETE /api/me/profile-image": "delete_profile_image",
    "POST /api/me/account-deletion": "delete_account",
    "POST /api/posts": "create_post",
    "GET /api/posting-window/current": "get_current_posting_window",
    "GET /api/posts/me/current": "get_my_current_post",
    "GET /api/posts/:postId": "get_post",
    "PATCH /api/posts/:postId": "update_post",
    "DELETE /api/posts/:postId": "delete_post",
    "GET /api/feeds/:feed": "get_feed",
    "GET /api/memories": "get_memories",
    "GET /api/memories/:date": "get_memories_by_date",
    "POST /api/posts/:postId/comments": "create_comment",
    "GET /api/posts/:postId/comments": "get_comments",
    "DELETE /api/comments/:commentId": "delete_comment",
    "POST /api/posts/:postId/realmojis": "replace_real_moji",
    "GET /api/posts/:postId/realmojis": "get_real_mojis",
    "DELETE /api/realmojis/:realMojiId": "delete_real_moji",
    "GET /api/images/:imageId": "get_image",
    "GET /api/notifications": "get_notifications",
    "POST /api/friend-requests": "create_friend_request",
    "POST /api/friend-requests/:requestId/:action": "transition_friend_request",
    "GET /api/relationships/:kind": "get_relationships",
    "DELETE /api/friendships/:userId": "delete_friendship",
    "PUT /api/follows/:userId": "follow",
    "DELETE /api/follows/:userId": "unfollow",
    "PUT /api/blocks/:userId": "block",
    "DELETE /api/blocks/:userId": "unblock",
    "POST /api/admin/auth/login": "admin_login",
    "POST /api/admin/auth/logout": "admin_logout",
    "PUT /api/admin/users/:userId/certification": "certify_user",
    "DELETE /api/admin/users/:userId/certification": "delete_user_certification",
    "POST /api/admin/posting-window": "open_posting_window",
}

LOG_LEVEL_RANKS = {"debug": 0, "info": 1, "error": 2}


def compile_route(route: str) -> tuple[str, re.Pattern[str]]:
    method, path = route.split(" ", 1)
    return method, re.compile(re.sub(r":\w+", "[^/]+", path))


ROUTE_PATTERNS = [
    (*compile_route(route), action) for route, action in ACTION_NAMES.items()
]


def action_name(scope: Scope) -> str | None:
    for method, pattern, action in ROUTE_PATTERNS:
        if method == scope["method"] and pattern.fullmatch(scope["path"]):
            return action
    return None


class ActionLogMiddleware:
    def __init__(self, app: ASGIApp, min_level: str, output: TextIO | None = None):
        self.app = app
        self.min_rank = LOG_LEVEL_RANKS.get(min_level, 0)
        self.output = output

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return
        state: dict[str, Any] = scope.setdefault("state", {})
        status = HTTPStatus.INTERNAL_SERVER_ERROR.value

        async def send_with_status(message: Message) -> None:
            nonlocal status
            if message["type"] == "http.response.start":
                status = message["status"]
            await send(message)

        try:
            await self.app(scope, receive, send_with_status)
        finally:
            self.write(scope, state, status)

    def write(self, scope: Scope, state: dict[str, Any], status: int) -> None:
        if status >= HTTPStatus.INTERNAL_SERVER_ERROR:
            level = "error"
        elif scope["method"] == "GET" and not (
            status == HTTPStatus.FORBIDDEN and state.get("abuse") == "invalid_signature"
        ):
            level = "debug"
        else:
            level = "info"
        if LOG_LEVEL_RANKS[level] < self.min_rank:
            return

        action = action_name(scope)
        session = state.get("session")
        entry: dict[str, Any] = {
            "level": level,
            "action": action,
            "status": status,
            "ip": client_ip(scope),
            "user_id": session.user_id if isinstance(session, Session) else None,
        }
        if action in ("login", "register"):
            username = state.get("username")
            valid = isinstance(username, str) and valid_username(username)
            entry["username"] = username if valid else None
        if abuse := state.get("abuse"):
            entry["abuse"] = abuse
        line = json.dumps(
            entry, ensure_ascii=False, separators=(",", ":"), sort_keys=True
        )
        output = self.output or sys.stdout
        output.write(line + "\n")
        output.flush()
