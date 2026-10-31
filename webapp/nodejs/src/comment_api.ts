import { randomUUID } from "node:crypto";
import type { Handler } from "hono";
import { APIError } from "./api_error.ts";
import { bind } from "./bind.ts";
import { canComment, getComment, selectCommentsWithAuthor, type Comment } from "./comment.ts";
import { must } from "./db.ts";
import { getPost } from "./post.ts";
import { maxID, minID } from "./relationship.ts";
import type { RelationshipUserResponse } from "./relationship_list_api.ts";
import { jsonResponse, noContent } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";
import { getSession } from "./session.ts";
import { getUser, validUserText, type User } from "./user.ts";
import { newRelationshipUserResponse } from "./user_read_api.ts";

const createCommentRequest = {
  postId: { param: "postId" },
  body: { json: "body" },
} as const;

export type CommentResponse = {
  id: string;
  author: RelationshipUserResponse;
  body: string;
  createdAt: Temporal.Instant;
};

export function newCommentResponse(comment: Comment, user: User): CommentResponse {
  return { id: comment.id, author: newRelationshipUserResponse(user), body: comment.body, createdAt: comment.createdAt };
}

export function createCommentHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, createCommentRequest);
    if (!validUserText(input.body, 1, 1000)) {
      throw new APIError(400, "invalid_interaction");
    }
    const actor = getSession(c)!.userID;
    const post = await getPost(h.db, input.postId);
    if (!post) {
      throw new APIError(403, "interaction_forbidden");
    }
    await using tx = await h.db.begin();
    for (const id of new Set([minID(actor, post.authorID), maxID(actor, post.authorID)])) {
      must(await tx.get(`SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`, id));
    }
    const allowed = await canComment(tx, actor, post.id);
    if (!allowed) {
      throw new APIError(403, "interaction_forbidden");
    }
    const id = randomUUID();
    await tx.exec(`INSERT INTO comments(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,$4,clock_timestamp())`, id, post.id, actor, input.body);
    const comment = must(await getComment(tx, id));
    const user = must(await getUser(tx, actor));
    await tx.commit();
    return jsonResponse(c, 201, newCommentResponse(comment, user));
  };
}

const getCommentsRequest = {
  postId: { param: "postId" },
  afterCreatedAt: { query: "afterCreatedAt", type: "time" },
  afterId: { query: "afterId" },
} as const;

type GetCommentsResponse = {
  items: CommentResponse[];
  hasMore: boolean;
};

export function getCommentsHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, getCommentsRequest);
    if ((input.afterCreatedAt === null) !== (input.afterId === "")) {
      throw new APIError(400, "invalid_pagination");
    }
    const allowed = await canComment(h.db, getSession(c)!.userID, input.postId);
    if (!allowed) {
      throw new APIError(403, "interaction_forbidden");
    }
    const comments = await selectCommentsWithAuthor(
      h.db,
      `AND c.post_id=$1 AND ($2::timestamptz IS NULL OR (c.created_at,c.id)>($2,$3)) ORDER BY c.created_at,c.id LIMIT 51`,
      input.postId,
      input.afterCreatedAt,
      input.afterId,
    );
    let items = comments.map((comment) => newCommentResponse(comment.comment, comment.author));
    const hasMore = items.length > 50;
    if (hasMore) {
      items = items.slice(0, 50);
    }
    const response: GetCommentsResponse = { items, hasMore };
    return jsonResponse(c, 200, response);
  };
}

const deleteCommentRequest = {
  commentId: { param: "commentId" },
} as const;

export function deleteCommentHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, deleteCommentRequest);
    const actor = getSession(c)!.userID;
    await using tx = await h.db.begin();
    must(await tx.get(`SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`, actor));
    const record = await tx.get<{ author_id: string; post_author_id: string }>(
      `SELECT c.author_id,p.author_id AS post_author_id FROM comments c JOIN posts p ON p.id=c.post_id WHERE c.id=$1 AND c.deleted_at IS NULL FOR UPDATE OF c`,
      input.commentId,
    );
    if (!record) {
      throw new APIError(404, "interaction_not_found");
    }
    if (actor !== record.author_id && actor !== record.post_author_id) {
      throw new APIError(403, "interaction_forbidden");
    }
    await tx.exec(`UPDATE comments SET deleted_at=clock_timestamp() WHERE id=$1`, input.commentId);
    await tx.commit();
    return noContent(c);
  };
}
