import { randomUUID } from "node:crypto";
import type { Handler } from "hono";
import { APIError } from "./api_error.ts";
import { bind } from "./bind.ts";
import { must } from "./db.ts";
import { signedImageURL } from "./image_signature.ts";
import { decodeJPEG } from "./jpeg.ts";
import { canView, getPost, getPostWithAuthor, loadViewerContext, validLocation, type Post, type PostLocation } from "./post.ts";
import { getPostingWindow } from "./posting_window.ts";
import { jsonResponse, noContent } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";
import { getSession } from "./session.ts";
import { now } from "./time.ts";
import { getUser, validUserText, type User } from "./user.ts";
import { newGetMeResponse } from "./user_api.ts";

const createPostRequest = {
  image: { form: "image", type: "file" },
  caption: { form: "caption" },
  visibility: { form: "visibility" },
  locationKind: { form: "location_kind" },
  latitude: { form: "latitude", type: "float" },
  longitude: { form: "longitude", type: "float" },
  locality: { form: "locality", type: "stringPointer" },
} as const;

type PostAuthorResponse = {
  id: string;
  username: string;
  displayName: string;
  profileImageUrl: string | null;
  bio: string;
  accountType: string;
};

type PostLocationResponse = {
  kind: string;
  latitude?: number;
  longitude?: number;
  locality?: string;
};

export type PostResponse = {
  id: string;
  author: PostAuthorResponse;
  postingWindowId: string;
  imageUrl: string;
  caption: string;
  visibility: string;
  location: PostLocationResponse;
  postedAt: Temporal.Instant;
  onTime: boolean;
  lateSeconds: number;
};

export function createPostHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, createPostRequest);
    if (input.image === null || !validUserText(input.caption, 0, 1000)) {
      throw new APIError(400, "invalid_post");
    }
    switch (input.visibility) {
      case "friends":
      case "friends_of_friends":
      case "public":
        break;
      default:
        throw new APIError(400, "invalid_post");
    }
    const location: PostLocation = {
      kind: input.locationKind,
      latitude: input.latitude,
      longitude: input.longitude,
      locality: input.locality,
    };
    if (!validLocation(location)) {
      throw new APIError(400, "invalid_post");
    }
    const imageData = Buffer.from(await input.image.arrayBuffer());
    const img = decodeJPEG(imageData);
    if (!img) {
      throw new APIError(400, "invalid_post");
    }
    if (img.width > 4096 || img.height > 4096) {
      throw new APIError(400, "invalid_post");
    }

    const userID = getSession(c)!.userID;
    await using tx = await h.db.begin();
    must(await tx.get(`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID));
    const user = must(await getUser(tx, userID));
    type WindowRow = { id: string; local_date: string; starts_at: Temporal.Instant };
    let window = await tx.get<WindowRow>(
      `SELECT id, local_date, starts_at FROM posting_windows WHERE region = $1 AND ended_at IS NULL FOR UPDATE`,
      user.region,
    );
    if (!window) {
      window = await tx.get<WindowRow>(
        `SELECT id, local_date, starts_at FROM posting_windows WHERE region = $1 AND ended_at IS NULL FOR UPDATE`,
        user.region,
      );
    }
    if (!window) {
      throw new APIError(409, "post_slot_unavailable");
    }
    const counts = must(
      await tx.get<{ active: number; deleted: number }>(
        `
   SELECT count(*) FILTER (WHERE deleted_at IS NULL) AS active,
          count(*) FILTER (WHERE deleted_at IS NOT NULL) AS deleted
   FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
   WHERE p.author_id = $1 AND w.local_date = $2
  `,
        userID,
        window.local_date,
      ),
    );
    const postedAt = now();
    if (counts.active !== 0 || counts.deleted >= 2 || Temporal.Instant.compare(postedAt, window.starts_at) < 0) {
      throw new APIError(409, "post_slot_unavailable");
    }
    const imageID = randomUUID();
    await tx.exec(
      `INSERT INTO image_objects (id, owner_user_id, data, media_type, created_at) VALUES ($1, $2, $3, 'image/jpeg', $4)`,
      imageID,
      userID,
      imageData,
      postedAt,
    );
    const postID = randomUUID();
    await tx.exec(
      `
   INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility,
                      location_kind, latitude, longitude, locality, posted_at)
   VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
  `,
      postID,
      userID,
      window.id,
      imageID,
      input.caption,
      input.visibility,
      location.kind,
      location.latitude,
      location.longitude,
      location.locality,
      postedAt,
    );
    const post = must(await getPost(tx, postID));
    await tx.commit();
    return jsonResponse(c, 201, newPostResponse(post, user));
  };
}

export function newPostResponse(post: Post, user: User): PostResponse {
  const response: PostResponse = {
    id: post.id,
    author: {
      id: user.id,
      username: user.username,
      displayName: user.displayName,
      profileImageUrl: newGetMeResponse(user).profileImageUrl,
      bio: user.bio,
      accountType: user.accountType,
    },
    postingWindowId: post.postingWindowID,
    imageUrl: signedImageURL(user, post.imageID),
    caption: post.caption,
    visibility: post.visibility,
    location: {
      kind: post.location.kind,
      latitude: post.location.latitude ?? undefined,
      longitude: post.location.longitude ?? undefined,
      locality: post.location.locality ?? undefined,
    },
    postedAt: post.postedAt,
    onTime: Temporal.Instant.compare(post.postedAt, post.onTimeDeadline) <= 0,
    lateSeconds: 0,
  };
  if (!response.onTime) {
    response.lateSeconds = post.postedAt.since(post.onTimeDeadline).seconds;
  }
  return response;
}

export function getMyCurrentPostHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const userID = getSession(c)!.userID;
    const row = await h.db.get<{ id: string }>(
      `
   SELECT p.id FROM posts p
   JOIN posting_windows w ON w.id = p.posting_window_id
   JOIN users u ON u.id = p.author_id
   JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                              AND current.local_date = w.local_date
   WHERE p.author_id = $1 AND p.deleted_at IS NULL
  `,
      userID,
    );
    if (!row) {
      throw new APIError(404, "post_not_found");
    }
    const post = await getPost(h.db, row.id);
    if (!post) {
      throw new APIError(404, "post_not_found");
    }
    const user = must(await getUser(h.db, userID));
    return jsonResponse(c, 200, newPostResponse(post, user));
  };
}

const getPostRequest = {
  postId: { param: "postId" },
} as const;

export function getPostHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, getPostRequest);
    const post = await getPostWithAuthor(h.db, input.postId);
    if (!post) {
      throw new APIError(404, "post_not_found");
    }
    const viewer = await loadViewerContext(h.db, getSession(c)!.userID);
    if (!viewer || !canView(viewer, post.post)) {
      throw new APIError(404, "post_not_found");
    }
    return jsonResponse(c, 200, newPostResponse(post.post, post.author));
  };
}

const updatePostRequest = {
  postId: { param: "postId" },
  caption: { json: "caption", type: "stringPointer" },
  visibility: { json: "visibility", type: "stringPointer" },
} as const;

export function updatePostHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, updatePostRequest);
    if (input.caption === null && input.visibility === null) {
      throw new APIError(400, "invalid_post");
    }
    if (input.caption !== null && !validUserText(input.caption, 0, 1000)) {
      throw new APIError(400, "invalid_post");
    }
    if (input.visibility !== null) {
      switch (input.visibility) {
        case "friends":
        case "friends_of_friends":
        case "public":
          break;
        default:
          throw new APIError(400, "invalid_post");
      }
    }
    const userID = getSession(c)!.userID;
    await using tx = await h.db.begin();
    must(await tx.get(`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID));
    let post = await getPost(tx, input.postId);
    if (!post) {
      throw new APIError(404, "post_not_found");
    }
    if (post.authorID !== userID) {
      throw new APIError(403, "post_not_editable");
    }
    const window = must(await getPostingWindow(tx, post.postingWindowID));
    if (window.endedAt) {
      throw new APIError(403, "post_not_editable");
    }
    if (input.caption !== null) {
      await tx.exec(`UPDATE posts SET caption = $2 WHERE id = $1`, post.id, input.caption);
    }
    if (input.visibility !== null) {
      await tx.exec(`UPDATE posts SET visibility = $2 WHERE id = $1`, post.id, input.visibility);
    }
    post = must(await getPost(tx, post.id));
    const user = must(await getUser(tx, userID));
    await tx.commit();
    return jsonResponse(c, 200, newPostResponse(post, user));
  };
}

const deletePostRequest = {
  postId: { param: "postId" },
} as const;

export function deletePostHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, deletePostRequest);
    const userID = getSession(c)!.userID;
    await using tx = await h.db.begin();
    must(await tx.get(`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID));
    const post = await getPost(tx, input.postId);
    if (!post) {
      throw new APIError(404, "post_not_found");
    }
    if (post.authorID !== userID) {
      throw new APIError(403, "post_not_editable");
    }
    const window = must(await getPostingWindow(tx, post.postingWindowID));
    const { count: deletions } = must(
      await tx.get<{ count: number }>(
        `
   SELECT count(*) FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
   WHERE p.author_id = $1 AND w.local_date = $2 AND p.deleted_at IS NOT NULL
  `,
        userID,
        window.localDate,
      ),
    );
    if (deletions >= 2) {
      throw new APIError(409, "post_slot_unavailable");
    }
    await tx.exec(`UPDATE posts SET deleted_at = clock_timestamp() WHERE id = $1`, post.id);
    await tx.commit();
    return noContent(c);
  };
}
