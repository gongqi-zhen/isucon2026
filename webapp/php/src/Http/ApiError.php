<?php

declare(strict_types=1);

namespace Isureal\Http;

/**
 * API のエラー。{"error": $error} を $status で返す
 */
final class ApiError extends \RuntimeException
{
    public function __construct(public readonly int $status, public readonly string $error)
    {
        parent::__construct($error);
    }

    /**
     * リクエストを解釈できなかったときのエラー
     */
    public static function badRequest(): self
    {
        return new self(400, 'code=400, message=Bad Request');
    }

    public static function unsupportedMediaType(): self
    {
        return new self(415, 'code=415, message=Unsupported Media Type');
    }
}
