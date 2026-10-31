<?php

declare(strict_types=1);

namespace Isureal\Http\Middleware;

use Isureal\Http\ApiError;
use Isureal\Model\AdminSession;
use Isureal\Repository\AdminSessionRepository;
use Psr\Http\Message\ResponseInterface;
use Psr\Http\Message\ServerRequestInterface;
use Psr\Http\Server\MiddlewareInterface;
use Psr\Http\Server\RequestHandlerInterface;

/**
 * 管理者のセッションを確かめ、リクエストの属性 admin_session に入れる
 */
final readonly class AdminSessionMiddleware implements MiddlewareInterface
{
    public const string ATTRIBUTE = 'admin_session';

    public function __construct(private AdminSessionRepository $sessions)
    {
    }

    public function process(ServerRequestInterface $request, RequestHandlerInterface $handler): ResponseInterface
    {
        $token = BearerToken::from($request, 'invalid_administrator_session');
        $session = $token === null ? null : $this->sessions->findByToken($token);
        if ($session === null) {
            throw new ApiError(401, 'invalid_administrator_session');
        }
        return $handler->handle($request->withAttribute(self::ATTRIBUTE, $session));
    }

    public static function session(ServerRequestInterface $request): AdminSession
    {
        return $request->getAttribute(self::ATTRIBUTE) ?? throw new ApiError(401, 'invalid_administrator_session');
    }
}
