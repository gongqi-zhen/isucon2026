<?php

declare(strict_types=1);

namespace Isureal\Http;

/**
 * JSON のリクエストボディ。値の型が違えば 400 にする
 */
final readonly class JsonBody
{
    /**
     * @param array<string, mixed> $data
     */
    public function __construct(private array $data = [])
    {
    }

    /**
     * 文字列の値。null または省略なら ''
     */
    public function string(string $key): string
    {
        return $this->optionalString($key) ?? '';
    }

    public function optionalString(string $key): ?string
    {
        $value = $this->data[$key] ?? null;
        if ($value !== null && !is_string($value)) {
            throw ApiError::badRequest();
        }
        return $value;
    }

    public function optionalInt(string $key): ?int
    {
        $value = $this->data[$key] ?? null;
        if ($value !== null && !is_int($value)) {
            throw ApiError::badRequest();
        }
        return $value;
    }
}
