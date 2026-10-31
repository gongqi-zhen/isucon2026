import { randomUUID } from "node:crypto";
import type { ContentfulStatusCode } from "hono/utils/http-status";
import { APIError } from "./api_error.ts";
import { must, type DB, type Queryer, type Row, type Tx } from "./db.ts";

export type FriendRequest = {
  id: string;
  requesterID: string;
  recipientID: string;
  status: string;
  createdAt: Temporal.Instant;
  updatedAt: Temporal.Instant;
};

function friendRequestFromRow(row: Row): FriendRequest {
  return {
    id: row.id,
    requesterID: row.requester_id,
    recipientID: row.recipient_id,
    status: row.status,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

export async function getFriendRequest(db: Queryer, id: string): Promise<FriendRequest | undefined> {
  const row = await db.get(`SELECT id, requester_id, recipient_id, status, created_at, updated_at FROM friend_requests WHERE id=$1`, id);
  return row === undefined ? undefined : friendRequestFromRow(row);
}

export function minID(a: string, b: string): string {
  return a < b ? a : b;
}

export function maxID(a: string, b: string): string {
  return a < b ? b : a;
}

export async function beginRelationshipTx(db: DB, actor: string, target: string): Promise<Tx> {
  if (actor === target || target === "") {
    throw new APIError(400, "invalid_relationship");
  }
  const tx = await db.begin();
  try {
    for (const id of [minID(actor, target), maxID(actor, target)]) {
      const locked = await tx.get(`SELECT id FROM users WHERE id=$1 AND status='active' FOR NO KEY UPDATE`, id);
      if (!locked) {
        throw new APIError(404, "relationship_not_found");
      }
    }
  } catch (err) {
    await tx[Symbol.asyncDispose]();
    throw err;
  }
  return tx;
}

export async function isBlocked(db: Queryer, actor: string, target: string): Promise<boolean> {
  const row = must(
    await db.get<{ exists: boolean }>(
      `SELECT EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$1 AND blocked_id=$2) OR (blocker_id=$2 AND blocked_id=$1))`,
      actor,
      target,
    ),
  );
  return row.exists;
}

export async function insertFriendship(tx: Tx, request: FriendRequest): Promise<void> {
  await tx.exec(
    `INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id) VALUES ($1,$2,$3,$4)`,
    randomUUID(),
    minID(request.requesterID, request.recipientID),
    maxID(request.requesterID, request.recipientID),
    request.id,
  );
}

export function requireRelationshipChange(count: number, status: ContentfulStatusCode, code: string): void {
  if (count === 0) {
    throw new APIError(status, code);
  }
}
