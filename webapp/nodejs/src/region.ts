import type { MiddlewareHandler } from "hono";
import { APIError } from "./api_error.ts";
import type { Env, HandlerContext } from "./server.ts";
import { getSession } from "./session.ts";

export function regionMiddleware(h: HandlerContext): MiddlewareHandler<Env> {
  return async (c, next) => {
    const session = getSession(c);
    if (!session) {
      throw new APIError(401, "invalid_session");
    }
    if (session.region !== h.config.region) {
      throw new APIError(403, "region_mismatch");
    }
    await next();
  };
}
