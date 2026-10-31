<?php

declare(strict_types=1);

namespace Isureal\Http\Middleware;

use Isureal\Http\ActionLogEntry;
use Isureal\Http\ApiError;
use Isureal\Model\LoginSession;
use Isureal\Repository\SessionRepository;
use Psr\Http\Message\ResponseInterface;
use Psr\Http\Message\ServerRequestInterface;
use Psr\Http\Server\MiddlewareInterface;
use Psr\Http\Server\RequestHandlerInterface;

/**
 * ログインセッションを確かめ、リクエストの属性 session に入れる
 */
final readonly class SessionMiddleware implements MiddlewareInterface
{
    public const string ATTRIBUTE = 'session';

    public function __construct(private SessionRepository $sessions)
    {
    }

    public function process(ServerRequestInterface $request, RequestHandlerInterface $handler): ResponseInterface
    {
        return $handler->handle($request->withAttribute(self::ATTRIBUTE, $this->authenticate($request)));
    }

    public function authenticate(ServerRequestInterface $request): LoginSession
    {
        $token = BearerToken::from($request, 'invalid_session');
        if ($token === null) {
            throw new ApiError(401, 'invalid_session');
        }
        $session = $this->sessions->findByToken($token) ?? throw new ApiError(401, 'invalid_session');
        ActionLogEntry::of($request)->userId = $session->userId;
        return $session;
    }

    public static function session(ServerRequestInterface $request): LoginSession
    {
        return $request->getAttribute(self::ATTRIBUTE) ?? throw new ApiError(401, 'invalid_session');
    }
}
