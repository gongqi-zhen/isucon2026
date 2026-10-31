<?php

declare(strict_types=1);

namespace Isureal\Http\Middleware;

use Isureal\Http\ApiError;
use Psr\Http\Message\ServerRequestInterface;

final class BearerToken
{
    /**
     * Authorization ヘッダのトークンを返す。ヘッダがなければ null
     */
    public static function from(ServerRequestInterface $request, string $invalidCode): ?string
    {
        $header = $request->getHeader('Authorization')[0] ?? '';
        if ($header === '') {
            return null;
        }
        if (!str_starts_with($header, 'Bearer ')) {
            throw new ApiError(401, $invalidCode);
        }
        return substr($header, strlen('Bearer '));
    }
}
