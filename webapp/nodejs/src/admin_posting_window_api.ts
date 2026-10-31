import { randomUUID } from "node:crypto";
import type { Handler } from "hono";
import { APIError } from "./api_error.ts";
import { bind } from "./bind.ts";
import { must } from "./db.ts";
import { getPostingWindow } from "./posting_window.ts";
import { jsonResponse } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";
import { validDate } from "./time.ts";
import { validRegion } from "./user.ts";

const openPostingWindowRequest = {
  region: { json: "region" },
  localDate: { json: "localDate" },
  onTimeSeconds: { json: "onTimeSeconds", type: "int" },
} as const;

type OpenPostingWindowResponse = {
  id: string;
  region: string;
  localDate: string;
  startsAt: Temporal.Instant;
  onTimeDeadline: Temporal.Instant;
};

export function openPostingWindowHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, openPostingWindowRequest);
    const region = input.region;
    const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(input.localDate);
    if (!validRegion(region) || match === null || !validDate(Number(match[1]), Number(match[2]), Number(match[3]))) {
      throw new APIError(400, "invalid_posting_window");
    }
    const localDate = input.localDate;
    let seconds = 24 * 60 * 60;
    if (input.onTimeSeconds !== null) {
      seconds = input.onTimeSeconds;
    }
    await using tx = await h.db.begin();
    await tx.exec(`LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE`);
    const { exists: conflict } = must(
      await tx.get<{ exists: boolean }>(`SELECT EXISTS(SELECT 1 FROM posting_windows WHERE region = $1 AND local_date >= $2)`, region, localDate),
    );
    if (conflict) {
      throw new APIError(409, "posting_window_conflict");
    }
    const { clock_timestamp: startsAt } = must(await tx.get<{ clock_timestamp: Temporal.Instant }>(`SELECT clock_timestamp()`));
    await tx.exec(`UPDATE posting_windows SET ended_at = $2 WHERE region = $1 AND ended_at IS NULL`, region, startsAt);
    const windowID = randomUUID();
    await tx.exec(
      `
   INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline)
   VALUES ($1, $2, $3, $4, $5)
  `,
      windowID,
      region,
      localDate,
      startsAt,
      startsAt.add({ seconds }),
    );
    await tx.exec(
      `
   INSERT INTO notifications (id, user_id, posting_window_id, created_at)
   SELECT gen_random_uuid()::text, id, $2, $3 FROM users WHERE region = $1 AND status = 'active'
  `,
      region,
      windowID,
      startsAt,
    );
    const window = must(await getPostingWindow(tx, windowID));
    await tx.commit();
    const response: OpenPostingWindowResponse = {
      id: window.id,
      region: window.region,
      localDate: window.localDate,
      startsAt: window.startsAt,
      onTimeDeadline: window.onTimeDeadline,
    };
    return jsonResponse(c, 201, response);
  };
}
