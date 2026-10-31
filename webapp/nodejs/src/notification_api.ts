import { setTimeout as sleep } from "node:timers/promises";
import type { Handler } from "hono";
import { must } from "./db.ts";
import { logError, marshalJSON } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";
import { getSession, type Session } from "./session.ts";
import { getUser } from "./user.ts";

type GetNotificationsResponse = {
  id: string;
  postingWindowId: string;
  region: string;
  createdAt: Temporal.Instant;
};

const notificationPollInterval = 500;

export function getNotificationsHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const session = getSession(c)!;
    const { clock_timestamp: connectedAt } = must(await h.db.get<{ clock_timestamp: Temporal.Instant }>(`SELECT clock_timestamp()`));
    const signal = c.req.raw.signal;
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        void streamNotification(h, session, connectedAt, signal, controller);
      },
    });
    return c.body(stream, 200, { "Content-Type": "text/event-stream" });
  };
}

async function streamNotification(
  h: HandlerContext,
  session: Session,
  connectedAt: Temporal.Instant,
  signal: AbortSignal,
  controller: ReadableStreamDefaultController<Uint8Array>,
): Promise<void> {
  const userID = session.userID;
  let nextTick = performance.now() + notificationPollInterval;
  try {
    for (;;) {
      let user;
      try {
        user = await getUser(h.db, userID);
      } catch (err) {
        if (!signal.aborted) {
          logError("failed to get notification recipient", err);
        }
        return;
      }
      if (!user || user.region !== session.region) {
        return;
      }
      let row;
      try {
        row = await h.db.get(
          `
    SELECT n.id, n.posting_window_id, w.region, n.created_at
    FROM notifications n JOIN posting_windows w ON w.id = n.posting_window_id
    WHERE n.user_id = $1 AND n.created_at > $2 AND w.ended_at IS NULL
    ORDER BY n.created_at, n.id LIMIT 1
   `,
          userID,
          connectedAt,
        );
      } catch (err) {
        if (!signal.aborted) {
          logError("failed to get notification", err);
        }
        return;
      }
      if (row) {
        const notification: GetNotificationsResponse = {
          id: row.id,
          postingWindowId: row.posting_window_id,
          region: row.region,
          createdAt: row.created_at,
        };
        try {
          controller.enqueue(new TextEncoder().encode(`data: ${marshalJSON(notification)}\n\n`));
        } catch {}
        return;
      }
      const now = performance.now();
      if (now < nextTick) {
        try {
          await sleep(nextTick - now, undefined, { signal });
        } catch {
          return;
        }
        nextTick += notificationPollInterval;
      } else {
        while (nextTick <= now) {
          nextTick += notificationPollInterval;
        }
      }
      if (signal.aborted) {
        return;
      }
    }
  } finally {
    try {
      controller.close();
    } catch {}
  }
}
