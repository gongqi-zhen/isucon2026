import type { Handler } from "hono";
import { APIError } from "./api_error.ts";
import { noContent } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";
import { getSession } from "./session.ts";

export function deleteAccountHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const userID = getSession(c)!.userID;
    await using tx = await h.db.begin();
    const locked = await tx.get(`SELECT id FROM users WHERE id=$1 AND status='active' FOR NO KEY UPDATE`, userID);
    if (!locked) {
      throw new APIError(404, "account_not_found");
    }
    await tx.exec(`UPDATE users SET status='deleted', deleted_at=clock_timestamp(), updated_at=clock_timestamp() WHERE id=$1`, userID);
    await tx.exec(
      `UPDATE login_sessions SET active=false, invalidated_at=clock_timestamp(), reason='account_deletion' WHERE user_id=$1 AND active=true`,
      userID,
    );
    await tx.commit();
    return noContent(c);
  };
}
