import type { Handler } from "hono";
import { APIError } from "./api_error.ts";
import { bind } from "./bind.ts";
import { noContent } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";

const certifyUserRequest = {
  userId: { param: "userId" },
} as const;

export function certifyUserHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, certifyUserRequest);
    const count = await h.db.exec(
      `UPDATE users SET account_type='certified_public', updated_at=clock_timestamp() WHERE id=$1 AND status='active' AND account_type='regular'`,
      input.userId,
    );
    if (count === 0) {
      throw new APIError(409, "certification_conflict");
    }
    return noContent(c);
  };
}

// 2026-08-26: 認定されたユーザーを戻すことは禁止する
export function deleteUserCertificationHandler(_: HandlerContext): Handler<Env> {
  return async () => {
    throw new APIError(409, "certification_irreversible");
  };
}
