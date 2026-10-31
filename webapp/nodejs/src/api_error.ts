import type { Context } from "hono";
import type { ContentfulStatusCode } from "hono/utils/http-status";
import { lookup } from "./lookup.ts";
import { jsonResponse, logError } from "./response.ts";

export class APIError extends Error {
  readonly status: ContentfulStatusCode;
  readonly code: string;

  constructor(status: ContentfulStatusCode, code: string) {
    super(code);
    this.status = status;
    this.code = code;
  }
}

const statusText = {
  400: "Bad Request",
  404: "Not Found",
  405: "Method Not Allowed",
  415: "Unsupported Media Type",
} as const;

export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export class HTTPError extends Error {
  readonly status: ContentfulStatusCode;

  constructor(status: ContentfulStatusCode, message: string = lookup(statusText, status) ?? "") {
    super(message);
    this.status = status;
  }

  static badRequest(err: unknown): HTTPError {
    return new HTTPError(400, `code=400, message=Bad Request, err=${errorMessage(err)}`);
  }

  static unsupportedMediaType(): HTTPError {
    return new HTTPError(415, "code=415, message=Unsupported Media Type");
  }
}

export function apiErrorHandler(err: Error, c: Context): Response {
  let status: ContentfulStatusCode = 500;
  let message = err.message;
  if (err instanceof APIError) {
    status = err.status;
    message = err.code;
  } else if (err instanceof HTTPError) {
    status = err.status;
  }
  if (status >= 500) {
    logError("request failed", err);
  }
  return jsonResponse(c, status, { error: message });
}
