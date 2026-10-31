import type { HttpBindings } from "@hono/node-server";
import { Hono, type NotFoundHandler } from "hono";
import { deleteAccountHandler } from "./account_deletion_api.ts";
import { actionLogMiddleware } from "./action_log.ts";
import { adminLoginHandler, adminLogoutHandler } from "./admin_api.ts";
import { openPostingWindowHandler } from "./admin_posting_window_api.ts";
import { adminSessionMiddleware, type AdminSession } from "./admin_session.ts";
import { certifyUserHandler, deleteUserCertificationHandler } from "./admin_user_api.ts";
import { apiErrorHandler, errorMessage } from "./api_error.ts";
import { createCommentHandler, deleteCommentHandler, getCommentsHandler } from "./comment_api.ts";
import type { Config } from "./config.ts";
import { DB } from "./db.ts";
import { exportPostsHandler } from "./export_api.ts";
import { getFeedHandler } from "./feed_api.ts";
import { createFriendRequestHandler, transitionFriendRequestHandler } from "./friend_request_api.ts";
import { getImageHandler } from "./image_api.ts";
import { initializeHandler } from "./initialize.ts";
import { IPBanList, ipBanMiddleware } from "./ip_ban.ts";
import { getMemoriesByDateHandler, getMemoriesHandler } from "./memory_api.ts";
import { getNotificationsHandler } from "./notification_api.ts";
import { createPostHandler, deletePostHandler, getMyCurrentPostHandler, getPostHandler, updatePostHandler } from "./post_api.ts";
import { getCurrentPostingWindowHandler } from "./posting_window_api.ts";
import { deleteProfileImageHandler, putProfileImageHandler } from "./profile_image_api.ts";
import { deleteRealMojiHandler, getRealMojisHandler, replaceRealMojiHandler } from "./realmoji_api.ts";
import { regionMiddleware } from "./region.ts";
import { blockHandler, deleteFriendshipHandler, followHandler, unblockHandler, unfollowHandler } from "./relationship_api.ts";
import { getRelationshipsHandler } from "./relationship_list_api.ts";
import { encodeJSON, jsonResponse } from "./response.ts";
import { sessionMiddleware, type Session } from "./session.ts";
import { validRegion } from "./user.ts";
import { getMeHandler, loginHandler, logoutHandler, registerHandler, updateMeHandler } from "./user_api.ts";
import { getUserHandler, searchUsersHandler } from "./user_read_api.ts";

export type HandlerContext = {
  db: DB;
  config: Config;
  ipBans: IPBanList;
};

export type Env = {
  Bindings: HttpBindings;
  Variables: {
    session: Session | null;
    adminSession: AdminSession | null;
    username: string;
    abuse: string;
  };
};

export async function newServer(config: Config, poolSize: number): Promise<Hono<Env>> {
  if (!validRegion(config.region)) {
    throw new Error("invalid server region: " + config.region);
  }
  const e = new Hono<Env>();

  e.use(actionLogMiddleware(config.logLevel, process.stdout));
  e.onError(apiErrorHandler);

  const db = new DB(config, poolSize);
  try {
    await db.ping();
  } catch (err) {
    throw new Error("failed to ping database: " + errorMessage(err));
  }

  const handlerContext: HandlerContext = {
    db,
    config,
    ipBans: new IPBanList(),
  };
  e.use(ipBanMiddleware(handlerContext));

  e.post("/api/initialize", initializeHandler(handlerContext));
  e.post("/api/auth/register", registerHandler(handlerContext));
  e.post("/api/auth/login", loginHandler(handlerContext));
  e.post("/api/auth/logout", sessionMiddleware(handlerContext, true), logoutHandler(handlerContext));
  e.get("/api/users", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), searchUsersHandler(handlerContext));
  e.get("/api/users/:userId", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), getUserHandler(handlerContext));
  e.get("/api/me/export", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), exportPostsHandler(handlerContext));
  e.get("/api/me", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), getMeHandler(handlerContext));
  e.patch("/api/me", sessionMiddleware(handlerContext, true), updateMeHandler(handlerContext));
  e.put("/api/me/profile-image", sessionMiddleware(handlerContext, true), putProfileImageHandler(handlerContext));
  e.delete("/api/me/profile-image", sessionMiddleware(handlerContext, true), deleteProfileImageHandler(handlerContext));
  e.post("/api/me/account-deletion", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), deleteAccountHandler(handlerContext));
  e.post("/api/posts", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), createPostHandler(handlerContext));
  e.get("/api/posting-window/current", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), getCurrentPostingWindowHandler(handlerContext));
  e.get("/api/posts/me/current", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), getMyCurrentPostHandler(handlerContext));
  e.get("/api/posts/:postId", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), getPostHandler(handlerContext));
  e.patch("/api/posts/:postId", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), updatePostHandler(handlerContext));
  e.delete("/api/posts/:postId", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), deletePostHandler(handlerContext));
  e.get("/api/feeds/:feed", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), getFeedHandler(handlerContext));
  e.get("/api/memories", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), getMemoriesHandler(handlerContext));
  e.get("/api/memories/:date", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), getMemoriesByDateHandler(handlerContext));
  e.post("/api/posts/:postId/comments", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), createCommentHandler(handlerContext));
  e.get("/api/posts/:postId/comments", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), getCommentsHandler(handlerContext));
  e.delete("/api/comments/:commentId", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), deleteCommentHandler(handlerContext));
  e.post("/api/posts/:postId/realmojis", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), replaceRealMojiHandler(handlerContext));
  e.get("/api/posts/:postId/realmojis", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), getRealMojisHandler(handlerContext));
  e.delete("/api/realmojis/:realMojiId", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), deleteRealMojiHandler(handlerContext));
  e.get("/api/images/:imageId", getImageHandler(handlerContext));
  e.get("/api/notifications", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), getNotificationsHandler(handlerContext));
  e.post("/api/friend-requests", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), createFriendRequestHandler(handlerContext));
  e.post("/api/friend-requests/:requestId/:action", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), transitionFriendRequestHandler(handlerContext));
  e.get("/api/relationships/:kind", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), getRelationshipsHandler(handlerContext));
  e.delete("/api/friendships/:userId", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), deleteFriendshipHandler(handlerContext));
  e.put("/api/follows/:userId", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), followHandler(handlerContext));
  e.delete("/api/follows/:userId", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), unfollowHandler(handlerContext));
  e.put("/api/blocks/:userId", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), blockHandler(handlerContext));
  e.delete("/api/blocks/:userId", sessionMiddleware(handlerContext, true), regionMiddleware(handlerContext), unblockHandler(handlerContext));
  e.post("/api/admin/auth/login", adminLoginHandler(handlerContext));
  e.post("/api/admin/auth/logout", adminSessionMiddleware(handlerContext, true), adminLogoutHandler(handlerContext));
  e.put("/api/admin/users/:userId/certification", adminSessionMiddleware(handlerContext, true), certifyUserHandler(handlerContext));
  e.delete("/api/admin/users/:userId/certification", adminSessionMiddleware(handlerContext, true), deleteUserCertificationHandler(handlerContext));
  e.post("/api/admin/posting-window", adminSessionMiddleware(handlerContext, true), openPostingWindowHandler(handlerContext));

  e.notFound(routeNotFoundHandler(e));

  return e;
}

const allowMethods = ["CONNECT", "DELETE", "GET", "HEAD", "PATCH", "POST", "PUT", "TRACE"] as const;

function routeNotFoundHandler(e: Hono<Env>): NotFoundHandler<Env> {
  let routes: { method: string; pattern: RegExp }[] | undefined;
  return (c) => {
    routes ??= e.routes
      .filter((route) => route.method !== "ALL")
      .map((route) => ({ method: route.method, pattern: new RegExp(`^${route.path.replace(/:[^/]+/g, "[^/]+")}$`) }));
    const path = c.req.path;
    const allowed = allowMethods.filter((method) => routes!.some((route) => route.method === method && route.pattern.test(path)));
    if (allowed.length === 0) {
      return jsonResponse(c, 404, { error: "Not Found" });
    }
    const allow = ["OPTIONS", ...allowed].join(", ");
    if (c.req.method === "OPTIONS") {
      return c.body(null, 204, { Allow: allow });
    }
    return c.body(encodeJSON({ error: "Method Not Allowed" }), 405, { "Content-Type": "application/json", Allow: allow });
  };
}
