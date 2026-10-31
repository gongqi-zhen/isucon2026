import cluster from "node:cluster";
import { serve } from "@hono/node-server";
import { configFromEnv, envOrDefault } from "./config.ts";
import { newServer } from "./server.ts";

const workers = Number(envOrDefault("ISUREAL_WORKERS", "2"));

if (cluster.isPrimary && workers > 1) {
  for (let i = 0; i < workers; i++) {
    cluster.fork();
  }
  cluster.on("exit", (worker, code, signal) => {
    console.error(`worker ${worker.process.pid} exited (code=${code}, signal=${signal})`);
    process.exit(1);
  });
} else {
  const config = configFromEnv();
  const server = await newServer(config, Number(envOrDefault("ISUREAL_DB_POOL_SIZE", "15")));
  serve({ fetch: server.fetch, port: 8080 });
}
