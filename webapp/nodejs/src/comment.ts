import type { Queryer, Row } from "./db.ts";
import { getPostWithAuthor, loadViewerContext, canView } from "./post.ts";
import { setImageKey, userColumnsAs, userFromRow, type User } from "./user.ts";

export type Comment = {
  id: string;
  postID: string;
  authorID: string;
  body: string;
  createdAt: Temporal.Instant;
  deletedAt: Temporal.Instant | null;
};

function commentFromRow(row: Row): Comment {
  return {
    id: row.id,
    postID: row.post_id,
    authorID: row.author_id,
    body: row.body,
    createdAt: row.created_at,
    deletedAt: row.deleted_at,
  };
}

export async function getComment(db: Queryer, id: string): Promise<Comment | undefined> {
  const row = await db.get(`SELECT id,post_id,author_id,body,created_at,deleted_at FROM comments WHERE id=$1`, id);
  return row === undefined ? undefined : commentFromRow(row);
}

export type CommentWithAuthor = {
  comment: Comment;
  author: User;
};

export async function selectCommentsWithAuthor(db: Queryer, conditions: string, ...args: unknown[]): Promise<CommentWithAuthor[]> {
  const rows = await db.select(
    `
  SELECT c.id, c.post_id, c.author_id, c.body, c.created_at, c.deleted_at, ${userColumnsAs("u", "author")}
  FROM comments c JOIN users u ON u.id = c.author_id AND u.status = 'active'
  WHERE c.deleted_at IS NULL ${conditions}`,
    ...args,
  );
  return rows.map((row) => {
    const author = userFromRow(row, "author");
    setImageKey(author);
    return { comment: commentFromRow(row), author };
  });
}

export async function canComment(db: Queryer, viewerID: string, postID: string): Promise<boolean> {
  const post = await getPostWithAuthor(db, postID);
  if (!post) {
    return false;
  }
  const viewer = await loadViewerContext(db, viewerID);
  if (!viewer) {
    return false;
  }
  if (!canView(viewer, post.post) || post.post.windowEndedAt) {
    return false;
  }
  if (viewer.user.id === post.post.authorID) {
    return viewer.hasPostedToday;
  }
  if (post.post.visibility === "public") {
    return true;
  }
  return viewer.friends.has(post.post.authorID);
}
