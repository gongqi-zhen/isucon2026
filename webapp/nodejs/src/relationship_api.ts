import { randomUUID } from "node:crypto";
import type { Handler } from "hono";
import { APIError } from "./api_error.ts";
import { bind } from "./bind.ts";
import { must } from "./db.ts";
import { beginRelationshipTx, isBlocked, maxID, minID, requireRelationshipChange } from "./relationship.ts";
import { noContent } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";
import { getSession } from "./session.ts";
import { getUser } from "./user.ts";

const deleteFriendshipRequest = {
  userId: { param: "userId" },
} as const;

export function deleteFriendshipHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, deleteFriendshipRequest);
    const actor = getSession(c)!.userID;
    await using tx = await beginRelationshipTx(h.db, actor, input.userId);
    const count = await tx.exec(`DELETE FROM friendships WHERE user_low_id=$1 AND user_high_id=$2`, minID(actor, input.userId), maxID(actor, input.userId));
    requireRelationshipChange(count, 409, "relationship_conflict");
    await tx.commit();
    return noContent(c);
  };
}

const followRequest = {
  userId: { param: "userId" },
} as const;

export function followHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, followRequest);
    const actor = getSession(c)!.userID;
    await using tx = await beginRelationshipTx(h.db, actor, input.userId);
    const target = must(await getUser(tx, input.userId));
    if (target.accountType !== "certified_public") {
      throw new APIError(409, "relationship_conflict");
    }
    const blocked = await isBlocked(tx, actor, target.id);
    if (blocked) {
      throw new APIError(409, "relationship_conflict");
    }
    const { exists } = must(
      await tx.get<{ exists: boolean }>(`SELECT EXISTS(SELECT 1 FROM follows WHERE follower_id=$1 AND followee_id=$2)`, actor, target.id),
    );
    if (exists) {
      throw new APIError(409, "relationship_conflict");
    }
    await tx.exec(`INSERT INTO follows (id, follower_id, followee_id) VALUES ($1,$2,$3)`, randomUUID(), actor, target.id);
    await tx.commit();
    return noContent(c);
  };
}

const unfollowRequest = {
  userId: { param: "userId" },
} as const;

export function unfollowHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, unfollowRequest);
    const actor = getSession(c)!.userID;
    await using tx = await beginRelationshipTx(h.db, actor, input.userId);
    const count = await tx.exec(`DELETE FROM follows WHERE follower_id=$1 AND followee_id=$2`, actor, input.userId);
    requireRelationshipChange(count, 409, "relationship_conflict");
    await tx.commit();
    return noContent(c);
  };
}

const blockRequest = {
  userId: { param: "userId" },
} as const;

export function blockHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, blockRequest);
    const actor = getSession(c)!.userID;
    await using tx = await beginRelationshipTx(h.db, actor, input.userId);
    const { exists } = must(
      await tx.get<{ exists: boolean }>(`SELECT EXISTS(SELECT 1 FROM block_relations WHERE blocker_id=$1 AND blocked_id=$2)`, actor, input.userId),
    );
    if (exists) {
      throw new APIError(409, "relationship_conflict");
    }
    await tx.exec(`INSERT INTO block_relations (id, blocker_id, blocked_id) VALUES ($1,$2,$3)`, randomUUID(), actor, input.userId);
    await tx.exec(`DELETE FROM friendships WHERE user_low_id=$1 AND user_high_id=$2`, minID(actor, input.userId), maxID(actor, input.userId));
    await tx.exec(`DELETE FROM follows WHERE (follower_id=$1 AND followee_id=$2) OR (follower_id=$2 AND followee_id=$1)`, actor, input.userId);
    await tx.exec(
      `UPDATE friend_requests SET status='cancelled', updated_at=clock_timestamp() WHERE status='pending' AND ((requester_id=$1 AND recipient_id=$2) OR (requester_id=$2 AND recipient_id=$1))`,
      actor,
      input.userId,
    );
    await tx.commit();
    return noContent(c);
  };
}

const unblockRequest = {
  userId: { param: "userId" },
} as const;

export function unblockHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, unblockRequest);
    const actor = getSession(c)!.userID;
    await using tx = await beginRelationshipTx(h.db, actor, input.userId);
    const count = await tx.exec(`DELETE FROM block_relations WHERE blocker_id=$1 AND blocked_id=$2`, actor, input.userId);
    requireRelationshipChange(count, 403, "relationship_action_forbidden");
    await tx.commit();
    return noContent(c);
  };
}
