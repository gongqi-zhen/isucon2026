import { randomUUID } from "node:crypto";
import type { Handler } from "hono";
import { APIError } from "./api_error.ts";
import { bind } from "./bind.ts";
import { must } from "./db.ts";
import { getFriendIDs } from "./post.ts";
import { beginRelationshipTx, getFriendRequest, insertFriendship, isBlocked } from "./relationship.ts";
import { jsonResponse } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";
import { getSession } from "./session.ts";

const createFriendRequestRequest = {
  recipientId: { json: "recipientId" },
} as const;

type FriendRequestResponse = {
  relationId: string;
  status: string;
  updatedAt: Temporal.Instant;
};

export function createFriendRequestHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, createFriendRequestRequest);
    const actor = getSession(c)!.userID;
    await using tx = await beginRelationshipTx(h.db, actor, input.recipientId);
    const blocked = await isBlocked(tx, actor, input.recipientId);
    if (blocked) {
      throw new APIError(409, "relationship_conflict");
    }
    const friends = await getFriendIDs(tx, actor);
    for (const id of friends) {
      if (id === input.recipientId) {
        throw new APIError(409, "relationship_conflict");
      }
    }
    const existing = await tx.get<{ id: string }>(
      `
   SELECT id FROM friend_requests WHERE status='pending' AND
    ((requester_id=$1 AND recipient_id=$2) OR (requester_id=$2 AND recipient_id=$1))
  `,
      actor,
      input.recipientId,
    );
    let requestID: string;
    if (existing) {
      const request = must(await getFriendRequest(tx, existing.id));
      if (request.requesterID === actor) {
        throw new APIError(409, "relationship_conflict");
      }
      await tx.exec(`UPDATE friend_requests SET status='accepted', updated_at=clock_timestamp() WHERE id=$1`, request.id);
      await insertFriendship(tx, request);
      requestID = request.id;
    } else {
      requestID = randomUUID();
      await tx.exec(
        `INSERT INTO friend_requests (id, requester_id, recipient_id, status, created_at, updated_at) VALUES ($1,$2,$3,'pending',clock_timestamp(),clock_timestamp())`,
        requestID,
        actor,
        input.recipientId,
      );
    }
    const request = must(await getFriendRequest(tx, requestID));
    await tx.commit();
    const response: FriendRequestResponse = { relationId: request.id, status: request.status, updatedAt: request.updatedAt };
    return jsonResponse(c, 201, response);
  };
}

const transitionFriendRequestRequest = {
  requestId: { param: "requestId" },
  action: { param: "action" },
} as const;

export function transitionFriendRequestHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, transitionFriendRequestRequest);
    let status: string;
    switch (input.action) {
      case "accept":
        status = "accepted";
        break;
      case "reject":
        status = "rejected";
        break;
      case "cancel":
        status = "cancelled";
        break;
      default:
        throw new APIError(403, "relationship_action_forbidden");
    }
    const actor = getSession(c)!.userID;
    let request = await getFriendRequest(h.db, input.requestId);
    if (!request) {
      throw new APIError(404, "relationship_not_found");
    }
    if ((status === "cancelled" && actor !== request.requesterID) || (status !== "cancelled" && actor !== request.recipientID)) {
      throw new APIError(403, "relationship_action_forbidden");
    }
    await using tx = await beginRelationshipTx(h.db, request.requesterID, request.recipientID);
    request = must(await getFriendRequest(tx, input.requestId));
    if (request.status !== "pending") {
      throw new APIError(403, "relationship_action_forbidden");
    }
    if (status === "accepted") {
      const blocked = await isBlocked(tx, request.requesterID, request.recipientID);
      if (blocked) {
        throw new APIError(409, "relationship_conflict");
      }
      await insertFriendship(tx, request);
    }
    await tx.exec(`UPDATE friend_requests SET status=$2, updated_at=clock_timestamp() WHERE id=$1`, request.id, status);
    request = must(await getFriendRequest(tx, request.id));
    await tx.commit();
    const response: FriendRequestResponse = { relationId: request.id, status: request.status, updatedAt: request.updatedAt };
    return jsonResponse(c, 200, response);
  };
}
