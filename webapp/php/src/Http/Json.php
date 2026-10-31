<?php

declare(strict_types=1);

namespace Isureal\Http;

use Psr\Http\Message\ResponseInterface;

final class Json
{
    public static function encode(mixed $data): string
    {
        return json_encode($data, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES | JSON_THROW_ON_ERROR);
    }

    public static function respond(ResponseInterface $response, mixed $data, int $status = 200): ResponseInterface
    {
        $response->getBody()->write(self::encode($data) . "\n");
        return $response->withStatus($status)->withHeader('Content-Type', 'application/json');
    }

    public static function noContent(ResponseInterface $response): ResponseInterface
    {
        return $response->withStatus(204);
    }
}
