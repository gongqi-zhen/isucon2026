<?php

declare(strict_types=1);

namespace Isureal;

final readonly class Config
{
    public function __construct(
        public string $region = 'asia',
        public string $dbHost = '127.0.0.1',
        public string $dbPort = '5432',
        public string $dbUser = 'isureal',
        public string $dbPassword = 'isureal',
        public string $dbName = 'isureal',
        public string $logLevel = 'debug',
    ) {
    }

    public static function fromEnv(): self
    {
        return new self(
            region: self::env('ISUREAL_REGION', 'asia'),
            dbHost: self::env('ISUREAL_DB_HOST', '127.0.0.1'),
            dbPort: self::env('ISUREAL_DB_PORT', '5432'),
            dbUser: self::env('ISUREAL_DB_USER', 'isureal'),
            dbPassword: self::env('ISUREAL_DB_PASSWORD', 'isureal'),
            dbName: self::env('ISUREAL_DB_NAME', 'isureal'),
            logLevel: self::env('ISUREAL_LOG_LEVEL', 'debug'),
        );
    }

    public function databaseDsn(): string
    {
        // 時刻は UTC で受け取る
        return sprintf(
            "pgsql:host=%s;port=%s;dbname=%s;options='-c TimeZone=UTC'",
            $this->dbHost,
            $this->dbPort,
            $this->dbName,
        );
    }

    private static function env(string $key, string $default): string
    {
        $value = getenv($key);
        return $value === false || $value === '' ? $default : $value;
    }
}
