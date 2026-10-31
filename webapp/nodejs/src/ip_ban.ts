import type { Context, MiddlewareHandler } from "hono";
import { APIError } from "./api_error.ts";
import type { DB } from "./db.ts";
import type { Env, HandlerContext } from "./server.ts";

const ipBanReloadInterval = 200;

type IPBanSnapshot = {
  loadedAt: number;
  bans: Map<string, Temporal.Instant>;
};

export class IPBanList {
  #reloading = false;
  #snapshot: IPBanSnapshot | null = null;

  async banned(db: DB, ip: string): Promise<boolean> {
    let snapshot = this.#snapshot;
    if ((snapshot === null || performance.now() - snapshot.loadedAt >= ipBanReloadInterval) && !this.#reloading) {
      this.#reloading = true;
      try {
        snapshot = await loadIPBans(db);
        this.#snapshot = snapshot;
      } finally {
        this.#reloading = false;
      }
    }
    if (!snapshot) {
      return false;
    }
    const until = snapshot.bans.get(ip);
    return until !== undefined && Temporal.Instant.compare(Temporal.Now.instant(), until) < 0;
  }
}

async function loadIPBans(db: DB): Promise<IPBanSnapshot> {
  const rows = await db.select<{ ip: string; banned_until: Temporal.Instant }>(
    `SELECT ip, banned_until FROM ip_bans WHERE banned_until > clock_timestamp()`,
  );
  const bans = new Map<string, Temporal.Instant>();
  for (const row of rows) {
    bans.set(row.ip, row.banned_until);
  }
  return { loadedAt: performance.now(), bans };
}

export function extractClientIP(c: Context<Env>): string {
  const ip = c.env.incoming.headersDistinct["x-client-ip"]?.[0];
  if (ip) {
    return ip;
  }
  return (c.env.incoming.socket.remoteAddress ?? "").replace(/^::ffff:(?=\d+\.\d+\.\d+\.\d+$)/, "");
}

export function ipBanMiddleware(h: HandlerContext): MiddlewareHandler<Env> {
  return async (c, next) => {
    const banned = await h.ipBans.banned(h.db, extractClientIP(c));
    if (banned) {
      throw new APIError(403, "ip_banned");
    }
    await next();
  };
}
