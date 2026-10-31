<?php

declare(strict_types=1);

namespace Isureal\Http\Middleware;

use Isureal\Http\ApiError;
use Isureal\Http\ClientIp;
use Isureal\Repository\IpBanRepository;
use Psr\Http\Message\ResponseInterface;
use Psr\Http\Message\ServerRequestInterface;
use Psr\Http\Server\MiddlewareInterface;
use Psr\Http\Server\RequestHandlerInterface;

final readonly class IpBanMiddleware implements MiddlewareInterface
{
    public function __construct(private IpBanRepository $ipBans)
    {
    }

    public function process(ServerRequestInterface $request, RequestHandlerInterface $handler): ResponseInterface
    {
        if ($this->ipBans->isBanned(ClientIp::of($request))) {
            throw new ApiError(403, 'ip_banned');
        }
        return $handler->handle($request);
    }
}
