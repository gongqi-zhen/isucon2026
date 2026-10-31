<?php

declare(strict_types=1);

namespace Isureal;

/**
 * bytea 列に渡すバイナリデータ
 */
final readonly class Bytea
{
    public function __construct(public string $data)
    {
    }

    /**
     * pdo_pgsql は bytea 列をストリームで返すので、文字列にする
     *
     * @param resource|string $value
     */
    public static function read(mixed $value): string
    {
        return is_string($value) ? $value : (string) stream_get_contents($value);
    }
}
