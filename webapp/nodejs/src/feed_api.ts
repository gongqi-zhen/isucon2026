import type { Handler } from "hono";
import { APIError } from "./api_error.ts";
import { bind } from "./bind.ts";
import type { Queryer } from "./db.ts";
import { canView, loadViewerContext, selectPostsWithAuthor, type Post, type PostWithAuthor, type ViewerContext } from "./post.ts";
import { newPostResponse, type PostResponse } from "./post_api.ts";
import { jsonResponse } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";
import { getSession } from "./session.ts";

const getFeedRequest = {
  feed: { param: "feed" },
  beforeScore: { query: "beforeScore", type: "int" },
  beforePostedAt: { query: "beforePostedAt", type: "time" },
  beforeId: { query: "beforeId" },
} as const;

type FeedItemResponse = PostResponse & {
  score?: number;
};

type GetFeedResponse = {
  items: FeedItemResponse[];
  hasMore: boolean;
};

export function getFeedHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, getFeedRequest);
    switch (input.feed) {
      case "friends":
      case "friends-of-friends":
      case "public":
      case "following":
        break;
      default:
        throw new APIError(400, "invalid_post");
    }
    if ((input.beforePostedAt === null) !== (input.beforeId === "")) {
      throw new APIError(400, "invalid_pagination");
    }
    const ranked = input.feed === "friends-of-friends";
    if (ranked && (input.beforeScore === null) !== (input.beforeId === "")) {
      throw new APIError(400, "invalid_pagination");
    }
    const viewer = await loadViewerContext(h.db, getSession(c)!.userID);
    if (!viewer) {
      const response: GetFeedResponse = { items: [], hasMore: false };
      return jsonResponse(c, 200, response);
    }
    const following = new Set<string>();
    if (input.feed === "following") {
      const followees = await h.db.select<{ followee_id: string }>(`SELECT followee_id FROM follows WHERE follower_id = $1`, viewer.user.id);
      for (const followee of followees) {
        following.add(followee.followee_id);
      }
    }
    const posts = await selectPostsWithAuthor(
      h.db,
      `
   AND p.author_id <> $1 AND w.ended_at IS NULL
   AND ($2::timestamptz IS NULL OR (p.posted_at, p.id) < ($2, $3))
   ORDER BY p.posted_at DESC, p.id DESC
  `,
      viewer.user.id,
      ranked ? null : input.beforePostedAt,
      ranked ? "" : input.beforeId,
    );
    const candidates: PostWithAuthor[] = [];
    for (const post of posts) {
      if (canView(viewer, post.post) && matchesFeed(viewer, following, post.post, input.feed)) {
        candidates.push(post);
      }
    }
    let scores = new Map<string, number>();
    if (ranked) {
      scores = await recommendationScores(h.db, viewer, candidates);
      candidates.sort((a, b) => scores.get(b.post.id)! - scores.get(a.post.id)!);
    }
    let items: FeedItemResponse[] = [];
    for (const post of candidates) {
      const score = scores.get(post.post.id) ?? 0;
      if (ranked && input.beforeScore !== null && !rankedBefore(score, post.post, input.beforeScore, input.beforePostedAt!, input.beforeId)) {
        continue;
      }
      const item: FeedItemResponse = newPostResponse(post.post, post.author);
      if (ranked) {
        item.score = score;
      }
      items.push(item);
      if (items.length === 51) {
        break;
      }
    }
    const hasMore = items.length > 50;
    if (hasMore) {
      items = items.slice(0, 50);
    }
    const response: GetFeedResponse = { items, hasMore };
    return jsonResponse(c, 200, response);
  };
}

function rankedBefore(score: number, post: Post, beforeScore: number, beforePostedAt: Temporal.Instant, beforeID: string): boolean {
  if (score !== beforeScore) {
    return score < beforeScore;
  }
  const order = Temporal.Instant.compare(post.postedAt, beforePostedAt);
  if (order !== 0) {
    return order < 0;
  }
  return post.id < beforeID;
}

async function recommendationScores(db: Queryer, viewer: ViewerContext, posts: PostWithAuthor[]): Promise<Map<string, number>> {
  const scores = new Map<string, number>();
  if (posts.length === 0) {
    return scores;
  }
  const authorIDs = posts.map((post) => post.post.authorID);
  const postIDs = posts.map((post) => post.post.id);

  const lowPlaceholders = authorIDs.map((_, i) => `$${i + 1}`).join(", ");
  const highPlaceholders = authorIDs.map((_, i) => `$${authorIDs.length + i + 1}`).join(", ");
  const friendships = await db.select<{ user_low_id: string; user_high_id: string }>(
    `SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id IN (${lowPlaceholders}) OR user_high_id IN (${highPlaceholders})`,
    ...authorIDs,
    ...authorIDs,
  );
  const connections = new Map<string, number>();
  for (const friendship of friendships) {
    connections.set(friendship.user_low_id, (connections.get(friendship.user_low_id) ?? 0) + (viewer.mutualFriends.get(friendship.user_high_id) ?? 0));
    connections.set(friendship.user_high_id, (connections.get(friendship.user_high_id) ?? 0) + (viewer.mutualFriends.get(friendship.user_low_id) ?? 0));
  }

  const realMojiPlaceholders = postIDs.map((_, i) => `$${i + 1}`).join(", ");
  const commentPlaceholders = postIDs.map((_, i) => `$${postIDs.length + i + 1}`).join(", ");
  const engagements = await db.select<{ post_id: string; user_id: string }>(
    `
  SELECT r.post_id, r.reactor_id AS user_id FROM realmojis r
  JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
  WHERE r.post_id IN (${realMojiPlaceholders}) AND r.replaced_at IS NULL AND r.deleted_at IS NULL
  UNION
  SELECT c.post_id, c.author_id AS user_id FROM comments c
  JOIN users u ON u.id = c.author_id AND u.status = 'active'
  WHERE c.post_id IN (${commentPlaceholders}) AND c.deleted_at IS NULL
 `,
    ...postIDs,
    ...postIDs,
  );
  const friendEngagements = new Map<string, number>();
  for (const engagement of engagements) {
    if (viewer.friends.has(engagement.user_id)) {
      friendEngagements.set(engagement.post_id, (friendEngagements.get(engagement.post_id) ?? 0) + 1);
    }
  }

  for (const post of posts) {
    const engagement = friendEngagements.get(post.post.id) ?? 0;
    const mutualFriends = viewer.mutualFriends.get(post.post.authorID) ?? 0;
    const connection = connections.get(post.post.authorID) ?? 0;
    scores.set(post.post.id, 10 * engagement + 3 * mutualFriends + Math.min(connection, 30));
  }
  return scores;
}

function matchesFeed(viewer: ViewerContext, following: Set<string>, post: Post, feed: string): boolean {
  switch (feed) {
    case "public":
      return post.visibility === "public";
    case "following":
      return following.has(post.authorID);
    case "friends":
      return viewer.friends.has(post.authorID);
    case "friends-of-friends":
      return !viewer.friends.has(post.authorID) && viewer.friendsOfFriends.has(post.authorID);
    default:
      return false;
  }
}
