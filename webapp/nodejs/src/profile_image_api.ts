import { randomUUID } from "node:crypto";
import type { Handler } from "hono";
import { APIError } from "./api_error.ts";
import { decodeJPEG } from "./jpeg.ts";
import { noContent } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";
import { getSession } from "./session.ts";

export function putProfileImageHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    if (c.req.header("Content-Type") !== "image/jpeg") {
      throw new APIError(415, "unsupported_media_type");
    }
    const data = Buffer.from(await c.req.arrayBuffer());
    const img = decodeJPEG(data);
    if (!img) {
      c.set("abuse", "invalid_image");
      throw new APIError(400, "invalid_profile_image");
    }
    if (img.width > 512 || img.height > 512) {
      throw new APIError(400, "invalid_profile_image");
    }

    const userID = getSession(c)!.userID;
    await using tx = await h.db.begin();
    const user = await tx.get<{ profile_image_id: string | null }>(`SELECT profile_image_id FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID);
    if (!user) {
      throw new APIError(404, "account_not_found");
    }
    const imageID = randomUUID();
    await tx.exec(`INSERT INTO image_objects(id,owner_user_id,data,media_type) VALUES ($1,$2,$3,'image/jpeg')`, imageID, userID, data);
    await tx.exec(`UPDATE users SET profile_image_id=$2,updated_at=clock_timestamp() WHERE id=$1`, userID, imageID);
    if (user.profile_image_id) {
      await tx.exec(`DELETE FROM image_objects WHERE id=$1`, user.profile_image_id);
    }
    await tx.commit();
    return noContent(c);
  };
}

export function deleteProfileImageHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const userID = getSession(c)!.userID;
    await using tx = await h.db.begin();
    const user = await tx.get<{ profile_image_id: string | null }>(`SELECT profile_image_id FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID);
    if (!user) {
      throw new APIError(404, "account_not_found");
    }
    if (!user.profile_image_id) {
      throw new APIError(404, "account_not_found");
    }
    await tx.exec(`UPDATE users SET profile_image_id=NULL,updated_at=clock_timestamp() WHERE id=$1`, userID);
    await tx.exec(`DELETE FROM image_objects WHERE id=$1`, user.profile_image_id);
    await tx.commit();
    return noContent(c);
  };
}
