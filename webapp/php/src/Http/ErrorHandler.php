<?php

declare(strict_types=1);

namespace Isureal\Http;

use Psr\Http\Message\ResponseFactoryInterface;
use Psr\Http\Message\ResponseInterface;
use Psr\Http\Message\ServerRequestInterface;
use Slim\Exception\HttpException;

/**
 * 例外を {"error": "..."} の応答にする
 */
final readonly class ErrorHandler
{
    public function __construct(private ResponseFactoryInterface $responseFactory)
    {
    }

    public function __invoke(ServerRequestInterface $request, \Throwable $exception): ResponseInterface
    {
        return self::render($this->responseFactory->createResponse(), $exception);
    }

    public static function render(ResponseInterface $response, \Throwable $exception): ResponseInterface
    {
        [$status, $message] = match (true) {
            $exception instanceof ApiError => [$exception->status, $exception->error],
            $exception instanceof HttpException => [$exception->getCode(), $response->withStatus($exception->getCode())->getReasonPhrase()],
            default => [500, $exception->getMessage()],
        };
        if ($status >= 500) {
            error_log('request failed: ' . $exception);
        }
        return Json::respond($response, ['error' => $message], $status);
    }
}
