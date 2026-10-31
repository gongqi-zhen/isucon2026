export type Config = {
  region: string;
  dbHost: string;
  dbPort: string;
  dbUser: string;
  dbPassword: string;
  dbName: string;
  logLevel: string;
};

export function configFromEnv(): Config {
  return {
    region: envOrDefault("ISUREAL_REGION", "asia"),
    dbHost: envOrDefault("ISUREAL_DB_HOST", "127.0.0.1"),
    dbPort: envOrDefault("ISUREAL_DB_PORT", "5432"),
    dbUser: envOrDefault("ISUREAL_DB_USER", "isureal"),
    dbPassword: envOrDefault("ISUREAL_DB_PASSWORD", "isureal"),
    dbName: envOrDefault("ISUREAL_DB_NAME", "isureal"),
    logLevel: envOrDefault("ISUREAL_LOG_LEVEL", "debug"),
  };
}

export function envOrDefault(key: string, defaultValue: string): string {
  const value = process.env[key];
  if (value) {
    return value;
  }
  return defaultValue;
}
