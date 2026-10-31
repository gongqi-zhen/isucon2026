import { randomUUID } from "node:crypto";
import type { Context, MiddlewareHandler } from "hono";
import { APIError } from "./api_error.ts";
import { must, type DB, type Queryer } from "./db.ts";
import type { Env, HandlerContext } from "./server.ts";
import { sha256Hex } from "./session.ts";

export type AdminSession = {
  token: string;
  administratorID: string;
  tokenHash: string;
  createdAt: Temporal.Instant;
};

export function getAdminSession(c: Context<Env>): AdminSession | null {
  return c.get("adminSession") ?? null;
}

async function getAdminSessionFromHeader(c: Context<Env>, db: DB): Promise<AdminSession | null> {
  const authHeader = c.req.header("Authorization") ?? "";
  if (authHeader === "") {
    return null;
  }
  if (!authHeader.startsWith("Bearer ")) {
    throw new APIError(401, "invalid_administrator_session");
  }
  const token = authHeader.slice("Bearer ".length);
  const row = await db.get(
    `
  SELECT administrator_id, token_hash, created_at
  FROM administrator_sessions
  WHERE token_hash = $1 AND active = true
 `,
    sha256Hex(token),
  );
  if (!row) {
    return null;
  }
  return { token: "", administratorID: row.administrator_id, tokenHash: row.token_hash, createdAt: row.created_at };
}

export function adminSessionMiddleware(h: HandlerContext, authRequired: boolean): MiddlewareHandler<Env> {
  return async (c, next) => {
    const session = await getAdminSessionFromHeader(c, h.db);
    if (session === null && authRequired) {
      throw new APIError(401, "invalid_administrator_session");
    }
    c.set("adminSession", session);
    await next();
  };
}

export async function createAdminSession(db: DB, administratorID: string): Promise<AdminSession> {
  await using tx = await db.begin();
  const sessionID = randomUUID();
  const token = randomUUID();
  await tx.exec(
    `
  INSERT INTO administrator_sessions (id, administrator_id, token_hash)
  VALUES ($1, $2, $3)
 `,
    sessionID,
    administratorID,
    sha256Hex(token),
  );
  const session = await getAdminSessionByID(tx, sessionID);
  await tx.commit();
  session.token = token;
  return session;
}

async function getAdminSessionByID(db: Queryer, sessionID: string): Promise<AdminSession> {
  const row = must(
    await db.get<{ administrator_id: string; token_hash: string; created_at: Temporal.Instant }>(`SELECT administrator_id, token_hash, created_at FROM administrator_sessions WHERE id = $1`, sessionID),
  );
  return { token: "", administratorID: row.administrator_id, tokenHash: row.token_hash, createdAt: row.created_at };
}

export async function invalidateAdminSession(db: DB, administratorID: string, tokenHash: string): Promise<void> {
  await db.exec(
    `
  UPDATE administrator_sessions
  SET active = false, invalidated_at = clock_timestamp()
  WHERE administrator_id = $1 AND token_hash = $2 AND active = true
 `,
    administratorID,
    tokenHash,
  );
}
