import { createHmac } from "node:crypto";
import { unix } from "./time.ts";
import type { User } from "./user.ts";

export const imageURLLifetime = 24 * 60 * 60;
const imageSignatureIterations = 1000;

export function imageSigningKey(secretKeyHash: string): Buffer {
  if (!/^[0-9A-Fa-f]{64}$/.test(secretKeyHash)) {
    throw new Error("invalid image signing key");
  }
  return Buffer.from(secretKeyHash, "hex");
}

export function imageSignature(key: Buffer, imageID: string, exp: number | string): Buffer {
  let value = Buffer.from(`${exp}:${imageID}`);
  for (let i = 0; i < imageSignatureIterations; i++) {
    value = createHmac("sha256", key).update(value).digest();
  }
  return value;
}

export function signedImageURL(user: User, imageID: string): string {
  const exp = unix(Temporal.Now.instant()) + imageURLLifetime;
  const sig = imageSignature(user.imageKey, imageID, exp).toString("base64url");
  return `/api/images/${pathEscape(imageID)}?exp=${exp}&sig=${sig}`;
}

function pathEscape(value: string): string {
  return Array.from(Buffer.from(value), (byte) => {
    const char = String.fromCharCode(byte);
    return /[A-Za-z0-9\-_.~$&+=:@]/.test(char) ? char : `%${byte.toString(16).toUpperCase().padStart(2, "0")}`;
  }).join("");
}
