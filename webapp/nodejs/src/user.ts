import type { Queryer, Row } from "./db.ts";
import { imageSigningKey } from "./image_signature.ts";

export type User = {
  id: string;
  username: string;
  secretKeyHash: string;
  displayName: string;
  bio: string;
  profileImageID: string | null;
  region: string;
  accountType: string;
  status: string;
  imageKey: Buffer;
};

export const usernamePattern = /^[a-z0-9_.-]{1,64}$/;

export function validUserText(value: string, minimum: number, maximum: number): boolean {
  const length = [...value].length;
  return value.isWellFormed() && length >= minimum && length <= maximum;
}

export function validRegion(region: string): boolean {
  switch (region) {
    case "america":
    case "europe":
    case "asia":
      return true;
    default:
      return false;
  }
}

export const userColumns = ["id", "username", "secret_key_hash", "display_name", "bio", "profile_image_id", "region", "account_type", "status"] as const;

export function userColumnsAs(alias: string, prefix: string): string {
  return userColumns.map((column) => `${alias}.${column} AS "${prefix}.${column}"`).join(", ");
}

export function userFromRow(row: Row, prefix?: string): User {
  const column = (name: string) => row[prefix === undefined ? name : `${prefix}.${name}`];
  return {
    id: column("id"),
    username: column("username"),
    secretKeyHash: column("secret_key_hash"),
    displayName: column("display_name"),
    bio: column("bio"),
    profileImageID: column("profile_image_id"),
    region: column("region"),
    accountType: column("account_type"),
    status: column("status"),
    imageKey: Buffer.alloc(0),
  };
}

export function setImageKey(user: User): void {
  user.imageKey = imageSigningKey(user.secretKeyHash);
}

export async function getUser(db: Queryer, userID: string): Promise<User | undefined> {
  const row = await db.get(`SELECT ${userColumns.join(", ")} FROM users WHERE id = $1 AND status = 'active'`, userID);
  if (!row) {
    return undefined;
  }
  const user = userFromRow(row);
  setImageKey(user);
  return user;
}
