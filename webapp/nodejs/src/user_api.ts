import { randomUUID } from "node:crypto";
import type { Handler } from "hono";
import { APIError } from "./api_error.ts";
import { bind } from "./bind.ts";
import { must } from "./db.ts";
import { signedImageURL } from "./image_signature.ts";
import { jsonResponse, noContent } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";
import { createLoginSessionTx, getSession, invalidateLoginSession, sha256Hex } from "./session.ts";
import { getUser, usernamePattern, userFromRow, validRegion, validUserText, type User } from "./user.ts";

const registerRequest = {
  username: { json: "username" },
  displayName: { json: "displayName" },
  region: { json: "region" },
} as const;

const loginRequest = {
  username: { json: "username" },
  secretKey: { json: "secretKey" },
} as const;

type SessionUserResponse = {
  id: string;
  username: string;
  displayName: string;
  bio: string;
  region: string;
  accountType: string;
  status: string;
};

type GetMeResponse = SessionUserResponse & {
  profileImageUrl: string | null;
};

const updateMeRequest = {
  displayName: { json: "displayName", type: "stringPointer" },
  bio: { json: "bio", type: "stringPointer" },
  region: { json: "region", type: "stringPointer" },
} as const;

function newSessionUserResponse(user: User): SessionUserResponse {
  return {
    id: user.id,
    username: user.username,
    displayName: user.displayName,
    bio: user.bio,
    region: user.region,
    accountType: user.accountType,
    status: user.status,
  };
}

export function newGetMeResponse(user: User): GetMeResponse {
  const response: GetMeResponse = { ...newSessionUserResponse(user), profileImageUrl: null };
  if (user.profileImageID) {
    response.profileImageUrl = signedImageURL(user, user.profileImageID);
  }
  return response;
}

export function registerHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, registerRequest);
    c.set("username", input.username);
    if (!usernamePattern.test(input.username) || !validUserText(input.displayName, 1, 100) || !validRegion(input.region)) {
      throw new APIError(400, "invalid_registration");
    }
    const secretKey = randomUUID();
    await using tx = await h.db.begin();
    // ユーザー名が一意である必要があります。
    await tx.exec(`LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE`);
    const { exists } = must(await tx.get<{ exists: boolean }>(`SELECT EXISTS(SELECT 1 FROM users WHERE username = $1)`, input.username));
    if (exists) {
      throw new APIError(409, "username_taken");
    }
    const userID = randomUUID();
    await tx.exec(
      `INSERT INTO users (id, username, secret_key_hash, display_name, region) VALUES ($1, $2, $3, $4, $5)`,
      userID,
      input.username,
      sha256Hex(secretKey),
      input.displayName,
      input.region,
    );
    const user = must(await getUser(tx, userID));
    await tx.commit();
    return jsonResponse(c, 201, { secretKey, user: newSessionUserResponse(user) });
  };
}

export function loginHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const input = await bind(c, loginRequest);
    c.set("username", input.username);
    if (!usernamePattern.test(input.username)) {
      throw new APIError(401, "invalid_credentials");
    }
    await using tx = await h.db.begin();
    const row = await tx.get(
      `SELECT id, username, secret_key_hash, display_name, bio, profile_image_id, region, account_type, status FROM users WHERE username = $1 FOR NO KEY UPDATE`,
      input.username,
    );
    if (!row) {
      throw new APIError(401, "invalid_credentials");
    }
    const user = userFromRow(row);
    if (user.status !== "active" || user.secretKeyHash !== sha256Hex(input.secretKey)) {
      throw new APIError(401, "invalid_credentials");
    }
    const session = await createLoginSessionTx(tx, user.id);
    await tx.commit();
    return jsonResponse(c, 200, { token: session.token, user: newSessionUserResponse(user) });
  };
}

export function logoutHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const session = getSession(c);
    if (!session) {
      throw new APIError(401, "invalid_session");
    }
    await invalidateLoginSession(h.db, session.userID, session.tokenHash);
    return noContent(c);
  };
}

export function getMeHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const session = getSession(c);
    if (!session) {
      throw new APIError(401, "invalid_session");
    }
    const user = await getUser(h.db, session.userID);
    if (!user) {
      throw new APIError(404, "account_not_found");
    }
    return jsonResponse(c, 200, newGetMeResponse(user));
  };
}

export function updateMeHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const session = getSession(c);
    if (!session) {
      throw new APIError(401, "invalid_session");
    }
    const input = await bind(c, updateMeRequest);
    if (
      (input.displayName === null && input.bio === null && input.region === null) ||
      (input.displayName !== null && !validUserText(input.displayName, 1, 100)) ||
      (input.bio !== null && !validUserText(input.bio, 0, 500)) ||
      (input.region !== null && !validRegion(input.region))
    ) {
      throw new APIError(400, "invalid_profile");
    }
    await using tx = await h.db.begin();
    if (input.displayName !== null) {
      await tx.exec(`UPDATE users SET display_name = $2, updated_at = clock_timestamp() WHERE id = $1`, session.userID, input.displayName);
    }
    if (input.bio !== null) {
      await tx.exec(`UPDATE users SET bio = $2, updated_at = clock_timestamp() WHERE id = $1`, session.userID, input.bio);
    }
    if (input.region !== null) {
      await tx.exec(`UPDATE users SET region = $2, updated_at = clock_timestamp() WHERE id = $1`, session.userID, input.region);
    }
    const user = await getUser(tx, session.userID);
    if (!user) {
      throw new APIError(404, "account_not_found");
    }
    await tx.commit();
    return jsonResponse(c, 200, newGetMeResponse(user));
  };
}
