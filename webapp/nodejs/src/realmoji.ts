import type { Queryer, Row } from "./db.ts";
import { setImageKey, userColumnsAs, userFromRow, type User } from "./user.ts";

export type RealMoji = {
  id: string;
  postID: string;
  reactorID: string;
  imageID: string;
  reactionType: string;
  createdAt: Temporal.Instant;
  replacedAt: Temporal.Instant | null;
  deletedAt: Temporal.Instant | null;
};

function realMojiFromRow(row: Row): RealMoji {
  return {
    id: row.id,
    postID: row.post_id,
    reactorID: row.reactor_id,
    imageID: row.image_id,
    reactionType: row.reaction_type,
    createdAt: row.created_at,
    replacedAt: row.replaced_at,
    deletedAt: row.deleted_at,
  };
}

export async function getRealMoji(db: Queryer, id: string): Promise<RealMoji | undefined> {
  const row = await db.get(
    `SELECT id, post_id, reactor_id, image_id, reaction_type, created_at, replaced_at, deleted_at FROM realmojis WHERE id=$1`,
    id,
  );
  return row === undefined ? undefined : realMojiFromRow(row);
}

export type RealMojiWithReactor = {
  realMoji: RealMoji;
  reactor: User;
};

export async function selectCurrentRealMojis(db: Queryer, conditions: string, ...args: unknown[]): Promise<RealMojiWithReactor[]> {
  const rows = await db.select(
    `
  SELECT r.id, r.post_id, r.reactor_id, r.image_id, r.reaction_type, r.created_at, r.replaced_at, r.deleted_at,
         ${userColumnsAs("u", "reactor")}
  FROM realmojis r JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
  WHERE r.replaced_at IS NULL AND r.deleted_at IS NULL ${conditions}`,
    ...args,
  );
  return rows.map((row) => {
    const reactor = userFromRow(row, "reactor");
    setImageKey(reactor);
    return { realMoji: realMojiFromRow(row), reactor };
  });
}
