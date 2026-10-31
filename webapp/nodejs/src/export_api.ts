import type { Handler } from "hono";
import JSZip from "jszip";
import { must } from "./db.ts";
import { postColumns, postFromRow } from "./post.ts";
import { newPostResponse, type PostResponse } from "./post_api.ts";
import { blobResponse, encodeJSON } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";
import { getSession } from "./session.ts";
import { getUser } from "./user.ts";

type ExportPostResponse = PostResponse & {
  imagePath: string;
};

export function exportPostsHandler(h: HandlerContext): Handler<Env> {
  return async (c) => {
    const userID = getSession(c)!.userID;
    const author = must(await getUser(h.db, userID));
    const posts = await h.db.select(
      `
   SELECT ${postColumns}, i.data AS image_data
   FROM posts p
   JOIN posting_windows w ON w.id = p.posting_window_id
   JOIN image_objects i ON i.id = p.image_id
   WHERE p.author_id = $1 AND p.deleted_at IS NULL
   ORDER BY p.posted_at, p.id
  `,
      userID,
    );

    const zip = new JSZip();
    const items: ExportPostResponse[] = [];
    for (const row of posts) {
      const imagePath = `images/${String(items.length + 1).padStart(6, "0")}.jpg`;
      zip.file(imagePath, row.image_data, { compression: "STORE", createFolders: false });
      items.push({ ...newPostResponse(postFromRow(row), author), imagePath });
    }
    zip.file("posts.json", encodeJSON(items), { compression: "DEFLATE" });
    const body = new Uint8Array(await zip.generateAsync({ type: "arraybuffer" }));
    c.header("Content-Disposition", `attachment; filename="isureal-posts.zip"`);
    c.header("Cache-Control", "no-store");
    return blobResponse(c, 200, "application/zip", body);
  };
}
