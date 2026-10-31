import { must, type Queryer, type Row } from "./db.ts";
import { getUser, setImageKey, userColumnsAs, userFromRow, validUserText, type User } from "./user.ts";

export type PostLocation = {
  kind: string;
  latitude: number | null;
  longitude: number | null;
  locality: string | null;
};

export function validLocation(location: PostLocation): boolean {
  switch (location.kind) {
    case "none":
      return location.latitude === null && location.longitude === null && location.locality === null;
    case "exact":
      return (
        location.latitude !== null &&
        location.longitude !== null &&
        location.locality === null &&
        location.latitude >= -90 &&
        location.latitude <= 90 &&
        location.longitude >= -180 &&
        location.longitude <= 180
      );
    case "approximate":
      return location.latitude === null && location.longitude === null && location.locality !== null && validUserText(location.locality, 1, 100);
    default:
      return false;
  }
}

export type Post = {
  id: string;
  authorID: string;
  postingWindowID: string;
  imageID: string;
  caption: string;
  visibility: string;
  location: PostLocation;
  postedAt: Temporal.Instant;
  onTimeDeadline: Temporal.Instant;
  windowEndedAt: Temporal.Instant | null;
};

export const postColumns = `p.id, p.author_id, p.posting_window_id, p.image_id, p.caption, p.visibility,
  p.location_kind, p.latitude, p.longitude, p.locality, p.posted_at,
  w.on_time_deadline, w.ended_at AS window_ended_at`;

export function postFromRow(row: Row): Post {
  return {
    id: row.id,
    authorID: row.author_id,
    postingWindowID: row.posting_window_id,
    imageID: row.image_id,
    caption: row.caption,
    visibility: row.visibility,
    location: { kind: row.location_kind, latitude: row.latitude, longitude: row.longitude, locality: row.locality },
    postedAt: row.posted_at,
    onTimeDeadline: row.on_time_deadline,
    windowEndedAt: row.window_ended_at,
  };
}

export async function getPost(db: Queryer, postID: string): Promise<Post | undefined> {
  const row = await db.get(
    `
  SELECT ${postColumns}
  FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
  WHERE p.id = $1 AND p.deleted_at IS NULL
 `,
    postID,
  );
  return row === undefined ? undefined : postFromRow(row);
}

export type PostWithAuthor = {
  post: Post;
  author: User;
};

export async function selectPostsWithAuthor(db: Queryer, conditions: string, ...args: unknown[]): Promise<PostWithAuthor[]> {
  const rows = await db.select(
    `
  SELECT ${postColumns}, ${userColumnsAs("u", "author")}
  FROM posts p
  JOIN posting_windows w ON w.id = p.posting_window_id
  JOIN users u ON u.id = p.author_id AND u.status = 'active'
  WHERE p.deleted_at IS NULL ${conditions}`,
    ...args,
  );
  return rows.map((row) => {
    const author = userFromRow(row, "author");
    setImageKey(author);
    return { post: postFromRow(row), author };
  });
}

export async function getPostWithAuthor(db: Queryer, postID: string): Promise<PostWithAuthor | undefined> {
  const posts = await selectPostsWithAuthor(db, `AND p.id = $1`, postID);
  return posts[0];
}

export type ViewerContext = {
  user: User;
  hasPostedToday: boolean;
  friends: Set<string>;
  friendsOfFriends: Set<string>;
  mutualFriends: Map<string, number>;
  blocked: Set<string>;
};

export async function loadViewerContext(db: Queryer, viewerID: string): Promise<ViewerContext | undefined> {
  const user = await getUser(db, viewerID);
  if (!user) {
    return undefined;
  }
  const viewer: ViewerContext = { user, hasPostedToday: false, friends: new Set(), friendsOfFriends: new Set(), mutualFriends: new Map(), blocked: new Set() };
  const posted = must(await db.get<{ exists: boolean }>(
    `
  SELECT EXISTS(
    SELECT 1 FROM posts p
    JOIN posting_windows posted ON posted.id = p.posting_window_id
    JOIN posting_windows current ON current.region = $2 AND current.ended_at IS NULL
                                AND current.local_date = posted.local_date
    WHERE p.author_id = $1 AND p.deleted_at IS NULL
  )
 `,
    viewer.user.id,
    viewer.user.region,
  ));
  viewer.hasPostedToday = posted.exists;
  viewer.blocked = await getBlockedUserIDs(db, viewer.user.id);
  const friends = await getFriendIDs(db, viewer.user.id);
  if (friends.length === 0) {
    return viewer;
  }
  for (const id of friends) {
    viewer.friends.add(id);
  }
  const lowPlaceholders = friends.map((_, i) => `$${i + 1}`).join(", ");
  const highPlaceholders = friends.map((_, i) => `$${friends.length + i + 1}`).join(", ");
  const friendships = await db.select<{ user_low_id: string; user_high_id: string }>(
    `SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id IN (${lowPlaceholders}) OR user_high_id IN (${highPlaceholders})`,
    ...friends,
    ...friends,
  );
  for (const friendship of friendships) {
    if (viewer.friends.has(friendship.user_low_id)) {
      viewer.friendsOfFriends.add(friendship.user_high_id);
      if (friendship.user_high_id !== viewer.user.id) {
        viewer.mutualFriends.set(friendship.user_high_id, (viewer.mutualFriends.get(friendship.user_high_id) ?? 0) + 1);
      }
    }
    if (viewer.friends.has(friendship.user_high_id)) {
      viewer.friendsOfFriends.add(friendship.user_low_id);
      if (friendship.user_low_id !== viewer.user.id) {
        viewer.mutualFriends.set(friendship.user_low_id, (viewer.mutualFriends.get(friendship.user_low_id) ?? 0) + 1);
      }
    }
  }
  return viewer;
}

export function canView(viewer: ViewerContext, post: Post): boolean {
  if (post.authorID === viewer.user.id) {
    return true;
  }
  if (post.windowEndedAt || !viewer.hasPostedToday || viewer.blocked.has(post.authorID)) {
    return false;
  }
  switch (post.visibility) {
    case "public":
      return true;
    case "friends":
      return viewer.friends.has(post.authorID);
    case "friends_of_friends":
      return viewer.friends.has(post.authorID) || viewer.friendsOfFriends.has(post.authorID);
    default:
      return false;
  }
}

export async function canViewPost(db: Queryer, viewerID: string, postID: string): Promise<boolean> {
  const post = await getPostWithAuthor(db, postID);
  if (!post) {
    return false;
  }
  const viewer = await loadViewerContext(db, viewerID);
  if (!viewer) {
    return false;
  }
  return canView(viewer, post.post);
}

export async function getFriendIDs(db: Queryer, userID: string): Promise<string[]> {
  const friendships = await db.select<{ user_low_id: string; user_high_id: string }>(
    `SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id = $1 OR user_high_id = $1`,
    userID,
  );
  const ids: string[] = [];
  for (const friendship of friendships) {
    if (friendship.user_low_id === userID) {
      ids.push(friendship.user_high_id);
    } else {
      ids.push(friendship.user_low_id);
    }
  }
  return ids;
}

export async function getBlockedUserIDs(db: Queryer, userID: string): Promise<Set<string>> {
  const rows = await db.select<{ id: string }>(
    `
  SELECT CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END AS id
  FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1
 `,
    userID,
  );
  return new Set(rows.map((row) => row.id));
}
