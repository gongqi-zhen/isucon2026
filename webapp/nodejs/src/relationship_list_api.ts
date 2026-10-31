import type { Handler } from "hono";
import { APIError } from "./api_error.ts";
import { bind } from "./bind.ts";
import { jsonResponse } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";
import { getSession } from "./session.ts";
import { setImageKey, userColumnsAs, userFromRow } from "./user.ts";
import { newRelationshipUserResponse } from "./user_read_api.ts";

const getRelationshipsRequest = {
  kind: { param: "kind" },
  beforeUpdatedAt: { query: "beforeUpdatedAt", type: "time" },
  beforeId: { query: "beforeId" },
} as const;

export type RelationshipUserResponse = {
  id: string;
  username: string;
  displayName: string;
  profileImageUrl: string | null;
  bio: string;
  accountType: string;
};

type RelationshipResponse = {
  relationId: string;
  user: RelationshipUserResponse;
  status: string;
  updatedAt: Temporal.Instant;
};

type GetRelationshipsResponse = {
  items: RelationshipResponse[];
  hasMore: boolean;
};

export function getRelationshipsHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, getRelationshipsRequest);
    if ((input.beforeUpdatedAt === null) !== (input.beforeId === "")) {
      throw new APIError(400, "invalid_pagination");
    }
    let query: string;
    switch (input.kind) {
      case "friends":
        query = `SELECT id, user_low_id AS first_id, user_high_id AS second_id, updated_at FROM friendships WHERE user_low_id=$1 OR user_high_id=$1`;
        break;
      case "following":
        query = `SELECT id, follower_id AS first_id, followee_id AS second_id, updated_at FROM follows WHERE follower_id=$1`;
        break;
      case "followers":
        query = `SELECT id, follower_id AS first_id, followee_id AS second_id, updated_at FROM follows WHERE followee_id=$1`;
        break;
      case "incoming-requests":
        query = `SELECT id, requester_id AS first_id, recipient_id AS second_id, updated_at FROM friend_requests WHERE recipient_id=$1 AND status='pending'`;
        break;
      case "outgoing-requests":
        query = `SELECT id, requester_id AS first_id, recipient_id AS second_id, updated_at FROM friend_requests WHERE requester_id=$1 AND status='pending'`;
        break;
      default:
        throw new APIError(400, "invalid_relationship");
    }
    const actor = getSession(c)!.userID;
    const records = await h.db.select(
      `
   SELECT relations.id, relations.updated_at, ${userColumnsAs("u", "user")}
   FROM (${query}) relations
   JOIN users u ON u.status = 'active'
               AND u.id = CASE WHEN relations.first_id = $1 THEN relations.second_id ELSE relations.first_id END
   WHERE ($2::timestamptz IS NULL OR (relations.updated_at, relations.id) < ($2, $3))
   ORDER BY relations.updated_at DESC, relations.id DESC LIMIT 51
  `,
      actor,
      input.beforeUpdatedAt,
      input.beforeId,
    );
    let items: RelationshipResponse[] = [];
    for (const record of records) {
      const user = userFromRow(record, "user");
      setImageKey(user);
      if (user.accountType !== "certified_public" && input.kind !== "friends") {
        user.bio = "";
      }
      let status: string = input.kind;
      if (input.kind === "incoming-requests" || input.kind === "outgoing-requests") {
        status = "pending";
      }
      items.push({
        relationId: record.id,
        user: newRelationshipUserResponse(user),
        status,
        updatedAt: record.updated_at,
      });
    }
    const hasMore = items.length > 50;
    if (hasMore) {
      items = items.slice(0, 50);
    }
    const response: GetRelationshipsResponse = { items, hasMore };
    return jsonResponse(c, 200, response);
  };
}
