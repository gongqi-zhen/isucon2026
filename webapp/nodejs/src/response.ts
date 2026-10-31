import type { Context } from "hono";
import type { ContentfulStatusCode } from "hono/utils/http-status";
import { lookup } from "./lookup.ts";

const jsonEscapes = {
  "<": "\\u003c",
  ">": "\\u003e",
  "&": "\\u0026",
  "\u2028": "\\u2028",
  "\u2029": "\\u2029",
} as const;

export function marshalJSON(value: unknown): string {
  return JSON.stringify(value).replace(/[<>&\u2028\u2029]/g, (char) => lookup(jsonEscapes, char) ?? char);
}

export function encodeJSON(value: unknown): string {
  return marshalJSON(value) + "\n";
}

export function jsonResponse(c: Context, status: ContentfulStatusCode, value: unknown): Response {
  return c.body(encodeJSON(value), status, { "Content-Type": "application/json" });
}

export function blobResponse(c: Context, status: ContentfulStatusCode, contentType: string, data: Uint8Array<ArrayBuffer>): Response {
  return c.body(data, status, { "Content-Type": contentType });
}

export function noContent(c: Context): Response {
  return c.body(null, 204);
}

export function logError(message: string, error: unknown): void {
  writeLog("ERROR", message, error instanceof Error ? error.message : String(error));
}

export function logWarn(message: string): void {
  writeLog("WARN", message);
}

function writeLog(level: string, message: string, error?: string): void {
  const entry: Record<string, string> = { time: Temporal.Now.instant().toString(), level, msg: message };
  if (error !== undefined) {
    entry.error = error;
  }
  process.stdout.write(JSON.stringify(entry) + "\n");
}
