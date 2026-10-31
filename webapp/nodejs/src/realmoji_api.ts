import { randomUUID } from "node:crypto";
import type { Handler } from "hono";
import { APIError } from "./api_error.ts";
import { bind } from "./bind.ts";
import { must } from "./db.ts";
import { signedImageURL } from "./image_signature.ts";
import { decodeJPEG } from "./jpeg.ts";
import { canViewPost, getPost } from "./post.ts";
import { getRealMoji, selectCurrentRealMojis, type RealMoji } from "./realmoji.ts";
import { beginRelationshipTx } from "./relationship.ts";
import type { RelationshipUserResponse } from "./relationship_list_api.ts";
import { jsonResponse, noContent } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";
import { getSession } from "./session.ts";
import { getUser, type User } from "./user.ts";
import { newRelationshipUserResponse } from "./user_read_api.ts";

const replaceRealMojiRequest = {
  postId: { param: "postId" },
  image: { form: "image", type: "file" },
  reactionType: { form: "reactionType" },
} as const;

export type RealMojiResponse = {
  id: string;
  reactor: RelationshipUserResponse;
  reactionType: string;
  imageUrl: string;
  createdAt: Temporal.Instant;
};

export function newRealMojiResponse(reaction: RealMoji, user: User): RealMojiResponse {
  return {
    id: reaction.id,
    reactor: newRelationshipUserResponse(user),
    reactionType: reaction.reactionType,
    imageUrl: signedImageURL(user, reaction.imageID),
    createdAt: reaction.createdAt,
  };
}

export function replaceRealMojiHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, replaceRealMojiRequest);
    if (input.image === null) {
      throw new APIError(400, "invalid_realmoji");
    }
    switch (input.reactionType) {
      case "like":
      case "smile":
      case "surprise":
      case "heart":
      case "laugh":
      case "instant":
        break;
      default:
        throw new APIError(400, "invalid_realmoji");
    }
    const data = Buffer.from(await input.image.arrayBuffer());
    const img = decodeJPEG(data);
    if (!img) {
      throw new APIError(400, "invalid_realmoji");
    }
    if (img.width > 512 || img.height > 512) {
      throw new APIError(400, "invalid_realmoji");
    }

    const actor = getSession(c)!.userID;
    const post = await getPost(h.db, input.postId);
    if (!post) {
      throw new APIError(403, "interaction_forbidden");
    }
    if (actor === post.authorID) {
      throw new APIError(403, "interaction_forbidden");
    }
    await using tx = await beginRelationshipTx(h.db, actor, post.authorID);
    const allowed = await canViewPost(tx, actor, post.id);
    if (!allowed) {
      throw new APIError(403, "interaction_forbidden");
    }
    await tx.exec(
      `UPDATE realmojis SET replaced_at=clock_timestamp() WHERE post_id=$1 AND reactor_id=$2 AND replaced_at IS NULL AND deleted_at IS NULL`,
      post.id,
      actor,
    );
    const imageID = randomUUID();
    await tx.exec(`INSERT INTO image_objects(id,owner_user_id,data,media_type) VALUES($1,$2,$3,'image/jpeg')`, imageID, actor, data);
    const id = randomUUID();
    await tx.exec(
      `INSERT INTO realmojis(id,post_id,reactor_id,image_id,reaction_type,created_at) VALUES($1,$2,$3,$4,$5,clock_timestamp())`,
      id,
      post.id,
      actor,
      imageID,
      input.reactionType,
    );
    const reaction = must(await getRealMoji(tx, id));
    const user = must(await getUser(tx, actor));
    await tx.commit();
    return jsonResponse(c, 201, newRealMojiResponse(reaction, user));
  };
}

const getRealMojisRequest = {
  postId: { param: "postId" },
  beforeCreatedAt: { query: "beforeCreatedAt", type: "time" },
  beforeId: { query: "beforeId" },
} as const;

type GetRealMojisResponse = {
  items: RealMojiResponse[];
  hasMore: boolean;
};

export function getRealMojisHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, getRealMojisRequest);
    if ((input.beforeCreatedAt === null) !== (input.beforeId === "")) {
      throw new APIError(400, "invalid_pagination");
    }
    const allowed = await canViewPost(h.db, getSession(c)!.userID, input.postId);
    if (!allowed) {
      throw new APIError(403, "interaction_forbidden");
    }
    const reactions = await selectCurrentRealMojis(
      h.db,
      `AND r.post_id=$1 AND ($2::timestamptz IS NULL OR (r.created_at,r.id)<($2,$3)) ORDER BY r.created_at DESC,r.id DESC LIMIT 51`,
      input.postId,
      input.beforeCreatedAt,
      input.beforeId,
    );
    let items = reactions.map((reaction) => newRealMojiResponse(reaction.realMoji, reaction.reactor));
    const hasMore = items.length > 50;
    if (hasMore) {
      items = items.slice(0, 50);
    }
    const response: GetRealMojisResponse = { items, hasMore };
    return jsonResponse(c, 200, response);
  };
}

const deleteRealMojiRequest = {
  realMojiId: { param: "realMojiId" },
} as const;

export function deleteRealMojiHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, deleteRealMojiRequest);
    const actor = getSession(c)!.userID;
    await using tx = await h.db.begin();
    must(await tx.get(`SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`, actor));
    const record = await tx.get<{ reactor_id: string; author_id: string }>(
      `SELECT r.reactor_id,p.author_id FROM realmojis r JOIN posts p ON p.id=r.post_id WHERE r.id=$1 AND r.deleted_at IS NULL FOR UPDATE OF r`,
      input.realMojiId,
    );
    if (!record) {
      throw new APIError(404, "interaction_not_found");
    }
    if (actor !== record.reactor_id && actor !== record.author_id) {
      throw new APIError(403, "interaction_forbidden");
    }
    await tx.exec(`UPDATE realmojis SET deleted_at=clock_timestamp() WHERE id=$1`, input.realMojiId);
    await tx.commit();
    return noContent(c);
  };
}
