import { createHash, randomUUID } from "node:crypto";
import type { Context, MiddlewareHandler } from "hono";
import { APIError } from "./api_error.ts";
import { must, type DB, type Queryer, type Tx } from "./db.ts";
import { logWarn } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";

export type Session = {
  region: string;
  token: string;
  userID: string;
  tokenHash: string;
  createdAt: Temporal.Instant;
};

export function getSession(c: Context<Env>): Session | null {
  return c.get("session") ?? null;
}

export function sha256Hex(value: string): string {
  return createHash("sha256").update(value).digest("hex");
}

async function getSessionFromHeader(c: Context<Env>, db: DB): Promise<Session | null> {
  const authHeader = c.req.header("Authorization") ?? "";
  if (authHeader === "") {
    return null;
  }
  if (!authHeader.startsWith("Bearer ")) {
    logWarn("Invalid authorization header format(not started with Bearer)");
    throw new APIError(401, "invalid_session");
  }

  const token = authHeader.slice("Bearer ".length);

  const tokenHash = sha256Hex(token);
  const row = await db.get(
    `
		SELECT s.user_id, s.token_hash, s.created_at, u.region
		FROM login_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.active = true AND u.status = 'active'
	`,
    tokenHash,
  );
  if (!row) {
    return null;
  }
  return { region: row.region, token: "", userID: row.user_id, tokenHash: row.token_hash, createdAt: row.created_at };
}

export function sessionMiddleware(handlerContext: HandlerContext, authRequired: boolean): MiddlewareHandler<Env> {
  return async (c, next) => {
    const session = await getSessionFromHeader(c, handlerContext.db);
    if (session === null && authRequired) {
      throw new APIError(401, "invalid_session");
    }
    c.set("session", session);
    await next();
  };
}

export async function invalidateLoginSession(db: DB, userID: string, tokenHash: string): Promise<void> {
  await using tx = await db.begin();

  must(await tx.get(`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID));

  await tx.exec(
    `
		UPDATE login_sessions
		SET active = false, invalidated_at = clock_timestamp(), reason = 'logout'
		WHERE user_id = $1 AND active = true AND token_hash = $2
	`,
    userID,
    tokenHash,
  );

  await tx.commit();
}

// 2026-09-03: リファクタリング後にこの関数の利用がないようです。消すのはいかがでしょうか
export async function createLoginSession(db: DB, userID: string): Promise<Session> {
  await using tx = await db.begin();
  const session = await createLoginSessionTx(tx, userID);
  await tx.commit();
  return session;
}

export async function createLoginSessionTx(tx: Tx, userID: string): Promise<Session> {
  const randomToken = randomUUID();
  const tokenHash = sha256Hex(randomToken);
  must(await tx.get(`SELECT id FROM users WHERE id = $1 AND status = 'active' FOR NO KEY UPDATE`, userID));
  await tx.exec(
    `
  UPDATE login_sessions
  SET active = false, invalidated_at = clock_timestamp(), reason = 'replaced'
  WHERE user_id = $1 AND active = true
 `,
    userID,
  );
  const sessionID = randomUUID();
  await tx.exec(
    `
  INSERT INTO login_sessions (id, user_id, token_hash)
  VALUES ($1, $2, $3)
 `,
    sessionID,
    userID,
    tokenHash,
  );
  const session = await getLoginSession(tx, sessionID);
  session.token = randomToken;
  return session;
}

async function getLoginSession(db: Queryer, sessionID: string): Promise<Session> {
  const row = must(
    await db.get<{ user_id: string; token_hash: string; created_at: Temporal.Instant }>(
      `SELECT user_id, token_hash, created_at FROM login_sessions WHERE id = $1`,
      sessionID,
    ),
  );
  return { region: "", token: "", userID: row.user_id, tokenHash: row.token_hash, createdAt: row.created_at };
}
