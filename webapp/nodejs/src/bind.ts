import type { Context } from "hono";
import { errorMessage, HTTPError } from "./api_error.ts";
import { parseRFC3339 } from "./time.ts";

type FieldTypes = {
  string: string;
  stringPointer: string | null;
  int: number | null;
  float: number | null;
  time: Temporal.Instant | null;
  file: File | null;
};

type FieldType = keyof FieldTypes;

export type Field = {
  param?: string;
  query?: string;
  json?: string;
  form?: string;
  type?: FieldType;
};

export type BindSpec = Record<string, Field>;

export type Bound<S extends BindSpec> = {
  -readonly [K in keyof S]: S[K] extends { type: infer T extends FieldType } ? FieldTypes[T] : string;
};

type Values = Map<string, string[]>;

const queryMethods = new Set(["GET", "DELETE", "HEAD"]);

export async function bind<S extends BindSpec>(c: Context, spec: S): Promise<Bound<S>> {
  const input: Record<string, unknown> = {};
  for (const [name, field] of Object.entries(spec)) {
    input[name] = (field.type ?? "string") === "string" ? "" : null;
  }
  bindValues(input, spec, "param", (key) => {
    const value = c.req.param(key);
    return value === undefined ? undefined : [value];
  });
  if (queryMethods.has(c.req.method)) {
    const { values } = parseQuery(rawQuery(c));
    bindValues(input, spec, "query", (key) => lookup(values, key));
  }
  await bindBody(c, input, spec);
  return input as Bound<S>;
}

export function rawQuery(c: Context): string {
  const url = c.req.url;
  const start = url.indexOf("?");
  return start < 0 ? "" : url.slice(start + 1);
}

export function parseQuery(raw: string): { values: Values; error: string | null } {
  const values: Values = new Map();
  let error: string | null = null;
  for (const pair of raw.split("&")) {
    if (pair.includes(";")) {
      error ??= "invalid semicolon separator in query";
      continue;
    }
    if (pair === "") {
      continue;
    }
    const separator = pair.indexOf("=");
    let key: string;
    let value: string;
    try {
      key = queryUnescape(separator < 0 ? pair : pair.slice(0, separator));
      value = queryUnescape(separator < 0 ? "" : pair.slice(separator + 1));
    } catch (err) {
      error ??= errorMessage(err);
      continue;
    }
    const list = values.get(key);
    if (list === undefined) {
      values.set(key, [value]);
    } else {
      list.push(value);
    }
  }
  return { values, error };
}

function queryUnescape(value: string): string {
  const invalid = /%(?![0-9A-Fa-f]{2})/.exec(value);
  if (invalid !== null) {
    throw new Error(`invalid URL escape ${JSON.stringify(value.slice(invalid.index, invalid.index + 3))}`);
  }
  value = value.replaceAll("+", " ");
  if (!value.includes("%")) {
    return value;
  }
  const parts = value.split(/(%[0-9A-Fa-f]{2})/);
  return Buffer.concat(
    parts.map((part) => (part.startsWith("%") ? Buffer.of(parseInt(part.slice(1), 16)) : Buffer.from(part))),
  ).toString("utf8");
}

function lookup(values: Values, key: string): string[] | undefined {
  const exact = values.get(key);
  if (exact !== undefined) {
    return exact;
  }
  const lowerKey = key.toLowerCase();
  for (const [name, list] of values) {
    if (name.toLowerCase() === lowerKey) {
      return list;
    }
  }
  return undefined;
}

function bindValues(
  input: Record<string, unknown>,
  spec: BindSpec,
  tag: "param" | "query" | "form",
  find: (key: string) => string[] | undefined,
): void {
  for (const [name, field] of Object.entries(spec)) {
    const key = field[tag];
    if (key === undefined) {
      continue;
    }
    const values = find(key);
    if (values === undefined || values.length === 0) {
      continue;
    }
    try {
      input[name] = convert(values[0], field.type ?? "string");
    } catch (err) {
      throw HTTPError.badRequest(`${key}: ${errorMessage(err)}`);
    }
  }
}

const floatPattern = /^[+-]?(?:(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?|inf(?:inity)?|nan)$/i;

function convert(value: string, type: FieldType): unknown {
  switch (type) {
    case "string":
    case "stringPointer":
      return value;
    case "time":
      return parseRFC3339(value);
    case "float":
      return parseFloat64(value);
    case "int":
      return parseInt64(value);
    default:
      throw new Error("unknown type");
  }
}

function parseInt64(value: string): number {
  if (value === "") {
    return 0;
  }
  if (!/^[+-]?\d+$/.test(value)) {
    throw new Error(`strconv.ParseInt: parsing ${JSON.stringify(value)}: invalid syntax`);
  }
  if (BigInt(value) >= 2n ** 63n || BigInt(value) < -(2n ** 63n)) {
    throw new Error(`strconv.ParseInt: parsing ${JSON.stringify(value)}: value out of range`);
  }
  return Number(value);
}

function parseFloat64(value: string): number {
  if (value === "") {
    return 0;
  }
  if (!floatPattern.test(value)) {
    throw new Error(`strconv.ParseFloat: parsing ${JSON.stringify(value)}: invalid syntax`);
  }
  const body = value.replace(/^[+-]/, "").toLowerCase();
  const sign = value.startsWith("-") ? -1 : 1;
  if (body === "nan") {
    return NaN;
  }
  if (body.startsWith("inf")) {
    return sign * Infinity;
  }
  const result = Number(value);
  if (!Number.isFinite(result)) {
    throw new Error(`strconv.ParseFloat: parsing ${JSON.stringify(value)}: value out of range`);
  }
  return result;
}

function hasBody(c: Context): boolean {
  const length = c.req.header("Content-Length");
  if (length !== undefined) {
    return Number(length) !== 0;
  }
  return c.req.header("Transfer-Encoding") !== undefined;
}

export function mediaType(c: Context): string {
  return (c.req.header("Content-Type") ?? "").split(";", 1)[0].trim();
}

async function bindBody(c: Context, input: Record<string, unknown>, spec: BindSpec): Promise<void> {
  if (!hasBody(c)) {
    return;
  }
  switch (mediaType(c)) {
    case "application/json":
      bindJSON(input, spec, Buffer.from(await c.req.arrayBuffer()).toString("utf8"));
      return;
    case "application/xml":
    case "text/xml":
      return;
    case "application/x-www-form-urlencoded": {
      const body = parseQuery(Buffer.from(await c.req.arrayBuffer()).toString("utf8"));
      if (body.error !== null) {
        throw HTTPError.badRequest(body.error);
      }
      for (const [key, list] of parseQuery(rawQuery(c)).values) {
        body.values.set(key, [...(body.values.get(key) ?? []), ...list]);
      }
      bindValues(input, spec, "form", (key) => lookup(body.values, key));
      return;
    }
    case "multipart/form-data":
      await bindMultipart(c, input, spec);
      return;
    default:
      throw HTTPError.unsupportedMediaType();
  }
}

function jsonType(value: unknown): string {
  if (value === null) {
    return "null";
  }
  if (Array.isArray(value)) {
    return "array";
  }
  switch (typeof value) {
    case "string":
      return "string";
    case "number":
      return `number ${value}`;
    case "boolean":
      return "bool";
    default:
      return "object";
  }
}

function bindJSON(input: Record<string, unknown>, spec: BindSpec, text: string): void {
  let data: unknown;
  try {
    data = JSON.parse(text);
  } catch (err) {
    throw HTTPError.badRequest(err);
  }
  if (data === null) {
    return;
  }
  if (typeof data !== "object" || Array.isArray(data)) {
    throw HTTPError.badRequest(`json: cannot unmarshal ${jsonType(data)} into request`);
  }
  const fields = new Map(Object.entries(data));
  for (const [name, field] of Object.entries(spec)) {
    const key = field.json;
    const value = key === undefined ? undefined : fields.get(key);
    if (key === undefined || value === undefined || value === null) {
      continue;
    }
    switch (field.type ?? "string") {
      case "string":
      case "stringPointer":
        if (typeof value !== "string") {
          throw HTTPError.badRequest(`json: cannot unmarshal ${jsonType(value)} into ${key} of type string`);
        }
        input[name] = value.toWellFormed();
        break;
      case "int":
        if (typeof value !== "number" || !Number.isInteger(value) || Math.abs(value) >= 2 ** 63) {
          throw HTTPError.badRequest(`json: cannot unmarshal ${jsonType(value)} into ${key} of type int64`);
        }
        input[name] = value;
        break;
      default:
        throw HTTPError.badRequest(`json: cannot unmarshal ${jsonType(value)} into ${key}`);
    }
  }
}

async function bindMultipart(c: Context, input: Record<string, unknown>, spec: BindSpec): Promise<void> {
  let form: FormData;
  try {
    form = await c.req.raw.formData();
  } catch (err) {
    throw HTTPError.badRequest(err);
  }
  const values: Values = new Map();
  const files = new Map<string, File[]>();
  for (const [key, value] of form) {
    if (typeof value === "string") {
      values.set(key, [...(values.get(key) ?? []), value]);
    } else {
      files.set(key, [...(files.get(key) ?? []), value]);
    }
  }
  const valueSpec: BindSpec = {};
  for (const [name, field] of Object.entries(spec)) {
    const file = field.type === "file" && field.form !== undefined ? files.get(field.form)?.[0] : undefined;
    if (file !== undefined) {
      input[name] = file;
    } else {
      valueSpec[name] = field;
    }
  }
  bindValues(input, valueSpec, "form", (key) => lookup(values, key));
}
