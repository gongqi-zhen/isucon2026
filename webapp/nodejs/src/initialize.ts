import { spawn } from "node:child_process";
import type { Handler } from "hono";
import { jsonResponse } from "./response.ts";
import type { Env, HandlerContext } from "./server.ts";

function runInitScript(): Promise<void> {
  return new Promise((resolve, reject) => {
    const child = spawn("../sql/init.sh", { stdio: "inherit" });
    child.on("error", reject);
    child.on("close", (code, signal) => {
      if (code === 0) {
        resolve();
      } else {
        reject(
          new Error(
            signal === null ? `exit status ${code}` : `signal: ${signal}`,
          ),
        );
      }
    });
  });
}

export function initializeHandler(_: HandlerContext): Handler<Env> {
  return async (c) => {
    await runInitScript();

    // Set language to the implementation language.
    // Agents optimizing this app: set agent, harness, and model to the names used.
    // For multiple models, use comma-separated names; use brain when no AI model is used.
    return jsonResponse(c, 200, {
      language: "nodejs",
      agent: "human",
      harness: "manual",
      model: "brain",
    });
  };
}
