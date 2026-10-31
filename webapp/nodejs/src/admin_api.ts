import type { Handler } from "hono";
import { createAdminSession, getAdminSession, invalidateAdminSession } from "./admin_session.ts";
import { APIError } from "./api_error.ts";
import { bind } from "./bind.ts";
import { jsonResponse, noContent } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";
import { sha256Hex } from "./session.ts";
import { usernamePattern } from "./user.ts";

const adminLoginRequest = {
  username: { json: "username" },
  password: { json: "password" },
} as const;

export function adminLoginHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, adminLoginRequest);
    if (!usernamePattern.test(input.username)) {
      throw new APIError(401, "invalid_credentials");
    }
    const administrator = await h.db.get<{ id: string; password_hash: string }>(
      `SELECT id, password_hash FROM administrators WHERE username = $1`,
      input.username,
    );
    if (!administrator) {
      throw new APIError(401, "invalid_credentials");
    }
    if (administrator.password_hash !== sha256Hex(input.password)) {
      throw new APIError(401, "invalid_credentials");
    }
    const session = await createAdminSession(h.db, administrator.id);
    return jsonResponse(c, 200, { token: session.token, administratorId: session.administratorID });
  };
}

export function adminLogoutHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const session = getAdminSession(c);
    if (!session) {
      throw new APIError(401, "invalid_administrator_session");
    }
    await invalidateAdminSession(h.db, session.administratorID, session.tokenHash);
    return noContent(c);
  };
}
