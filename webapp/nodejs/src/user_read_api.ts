import type { Handler } from "hono";
import { APIError } from "./api_error.ts";
import { bind } from "./bind.ts";
import { getBlockedUserIDs, getFriendIDs } from "./post.ts";
import { isBlocked } from "./relationship.ts";
import type { RelationshipUserResponse } from "./relationship_list_api.ts";
import { jsonResponse } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";
import { getSession } from "./session.ts";
import { getUser, setImageKey, userColumns, userFromRow, validUserText, type User } from "./user.ts";
import { newGetMeResponse } from "./user_api.ts";

const searchUsersRequest = {
  q: { query: "q" },
  beforeUsername: { query: "beforeUsername" },
  beforeId: { query: "beforeId" },
} as const;

type SearchUsersResponse = {
  items: RelationshipUserResponse[];
  hasMore: boolean;
};

function normalizeSearchText(value: string): string {
  return value.normalize("NFC").toUpperCase().toLowerCase().normalize("NFC");
}

export function searchUsersHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, searchUsersRequest);
    if (!validUserText(input.q, 1, 100)) {
      throw new APIError(400, "invalid_relationship");
    }
    if ((input.beforeUsername === "") !== (input.beforeId === "")) {
      throw new APIError(400, "invalid_pagination");
    }
    const viewer = getSession(c)!.userID;
    const rows = await h.db.select(
      `SELECT ${userColumns.join(", ")} FROM users WHERE status='active' AND id<>$1 AND ($2='' OR (username,id)>($2,$3)) ORDER BY username, id`,
      viewer,
      input.beforeUsername,
      input.beforeId,
    );
    const blocked = await getBlockedUserIDs(h.db, viewer);
    const query = normalizeSearchText(input.q);
    let items: RelationshipUserResponse[] = [];
    for (const row of rows) {
      const user = userFromRow(row);
      if (!normalizeSearchText(user.username).includes(query) && !normalizeSearchText(user.displayName).includes(query)) {
        continue;
      }
      if (blocked.has(user.id)) {
        continue;
      }
      setImageKey(user);
      if (user.accountType !== "certified_public") {
        user.bio = "";
      }
      items.push(newRelationshipUserResponse(user));
      if (items.length === 51) {
        break;
      }
    }
    const hasMore = items.length > 50;
    if (hasMore) {
      items = items.slice(0, 50);
    }
    const response: SearchUsersResponse = { items, hasMore };
    return jsonResponse(c, 200, response);
  };
}

const getUserRequest = {
  userId: { param: "userId" },
} as const;

export function getUserHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, getUserRequest);
    const viewer = getSession(c)!.userID;
    const user = await getUser(h.db, input.userId);
    if (!user) {
      throw new APIError(404, "relationship_not_found");
    }
    const blocked = await isBlocked(h.db, viewer, user.id);
    if (blocked) {
      throw new APIError(404, "relationship_not_found");
    }
    const friends = await getFriendIDs(h.db, viewer);
    if (user.accountType !== "certified_public" && !friends.includes(user.id)) {
      user.bio = "";
    }
    return jsonResponse(c, 200, newRelationshipUserResponse(user));
  };
}

export function newRelationshipUserResponse(user: User): RelationshipUserResponse {
  return {
    id: user.id,
    username: user.username,
    displayName: user.displayName,
    profileImageUrl: newGetMeResponse(user).profileImageUrl,
    bio: user.bio,
    accountType: user.accountType,
  };
}
