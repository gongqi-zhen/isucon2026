import type { Handler } from "hono";
import { jsonResponse } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";
import { getSession } from "./session.ts";

type CurrentPostingWindowResponse = {
  id: string;
  region: string;
  localDate: string;
  startsAt: Temporal.Instant;
  onTimeDeadline: Temporal.Instant;
};

type GetCurrentPostingWindowResponse = {
  window: CurrentPostingWindowResponse | null;
  hasPosted: boolean;
  canPost: boolean;
  deletionCount: number;
};

export function getCurrentPostingWindowHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const state = await h.db.get(
      `
   SELECT w.id, w.region, w.local_date, w.starts_at, w.on_time_deadline, w.ended_at,
          count(p.id) FILTER (WHERE p.deleted_at IS NULL) AS active,
          count(p.id) FILTER (WHERE p.deleted_at IS NOT NULL) AS deleted
   FROM users u JOIN posting_windows w ON w.region = u.region AND w.ended_at IS NULL
   LEFT JOIN posting_windows posted_window ON posted_window.local_date = w.local_date
   LEFT JOIN posts p ON p.author_id = u.id AND p.posting_window_id = posted_window.id
   WHERE u.id = $1
   GROUP BY w.id
  `,
      getSession(c)!.userID,
    );
    if (!state) {
      const response: GetCurrentPostingWindowResponse = { window: null, hasPosted: false, canPost: false, deletionCount: 0 };
      return jsonResponse(c, 200, response);
    }
    const startsAt: Temporal.Instant = state.starts_at;
    const response: GetCurrentPostingWindowResponse = {
      window: {
        id: state.id,
        region: state.region,
        localDate: state.local_date,
        startsAt,
        onTimeDeadline: state.on_time_deadline,
      },
      hasPosted: state.active > 0,
      canPost: state.active === 0 && state.deleted < 2 && Temporal.Instant.compare(Temporal.Now.instant(), startsAt) >= 0,
      deletionCount: state.deleted,
    };
    return jsonResponse(c, 200, response);
  };
}
