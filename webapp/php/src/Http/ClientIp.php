<?php

declare(strict_types=1);

namespace Isureal\Http;

use Psr\Http\Message\ServerRequestInterface;

final class ClientIp
{
    public static function of(ServerRequestInterface $request): string
    {
        $ip = $request->getHeader('X-Client-IP')[0] ?? '';
        if ($ip !== '') {
            return $ip;
        }
        $remoteAddr = $request->getServerParams()['REMOTE_ADDR'] ?? '';
        return is_string($remoteAddr) ? $remoteAddr : '';
    }
}
