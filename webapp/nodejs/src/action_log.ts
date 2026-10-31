import type { MiddlewareHandler } from "hono";
import { matchedRoutes } from "hono/route";
import { extractClientIP } from "./ip_ban.ts";
import { lookup } from "./lookup.ts";
import { encodeJSON } from "./response.ts";
import type { Env } from "./server.ts";
import { getSession } from "./session.ts";
import { usernamePattern } from "./user.ts";

const actionNames = {
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
} as const;

const logLevelRanks = { debug: 0, info: 1, error: 2 } as const;

export function actionLogMiddleware(minLevel: string, output: NodeJS.WritableStream): MiddlewareHandler<Env> {
  return async (c, next) => {
    await next();
    const status = c.res.status;

    let level: keyof typeof logLevelRanks = "info";
    if (status >= 500) {
      level = "error";
    } else if (c.req.method === "GET" && !(status === 403 && c.get("abuse") === "invalid_signature")) {
      level = "debug";
    }
    if (logLevelRanks[level] < (lookup(logLevelRanks, minLevel) ?? 0)) {
      return;
    }

    const route = matchedRoutes(c).find((route) => route.method !== "ALL");
    const action = route ? (lookup(actionNames, `${route.method} ${route.path}`) ?? null) : null;
    const entry: Record<string, unknown> = {
      action,
      ip: extractClientIP(c),
      level,
      status,
      user_id: getSession(c)?.userID ?? null,
    };
    if (action === "login" || action === "register") {
      entry.username = null;
      const username = c.get("username");
      if (typeof username === "string" && usernamePattern.test(username)) {
        entry.username = username;
      }
    }
    const abuse = c.get("abuse");
    if (abuse) {
      entry.abuse = abuse;
    }
    output.write(encodeJSON(entry));
  };
}
