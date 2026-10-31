<?php

declare(strict_types=1);

namespace Isureal\Http\Middleware;

use Isureal\Config;
use Isureal\Http\ApiError;
use Psr\Http\Message\ResponseInterface;
use Psr\Http\Message\ServerRequestInterface;
use Psr\Http\Server\MiddlewareInterface;
use Psr\Http\Server\RequestHandlerInterface;

/**
 * セッションのユーザーがこのサーバーのリージョンのユーザーか確かめる。SessionMiddleware の後に置く
 */
final readonly class RegionMiddleware implements MiddlewareInterface
{
    public function __construct(private Config $config)
    {
    }

    public function process(ServerRequestInterface $request, RequestHandlerInterface $handler): ResponseInterface
    {
        if (SessionMiddleware::session($request)->region !== $this->config->region) {
            throw new ApiError(403, 'region_mismatch');
        }
        return $handler->handle($request);
    }
}
