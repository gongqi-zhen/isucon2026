<?php

declare(strict_types=1);

namespace Isureal\Http;

use DateTimeImmutable;
use Isureal\Time;
use Psr\Http\Message\ServerRequestInterface;
use Psr\Http\Message\UploadedFileInterface;

/**
 * リクエストのクエリ、JSON、フォームを取り出す
 */
final class Input
{
    /**
     * JSON のリクエストボディを読む。ボディが空なら値のないものとして扱う
     */
    public static function json(ServerRequestInterface $request): JsonBody
    {
        $body = (string) $request->getBody();
        if ($body === '') {
            return new JsonBody();
        }
        if (self::mediaType($request) !== 'application/json') {
            throw ApiError::unsupportedMediaType();
        }
        return new JsonBody(self::decodeJsonObject($body));
    }

    /**
     * multipart/form-data などのフォームを取り出す。
     *
     * @return array{array<string, string>, array<string, UploadedFileInterface>} フィールドとファイル
     */
    public static function form(ServerRequestInterface $request): array
    {
        if (!self::hasBody($request)) {
            return [[], []];
        }
        switch (self::mediaType($request)) {
            case 'multipart/form-data':
            case 'application/x-www-form-urlencoded':
                $fields = [];
                foreach ((array) $request->getParsedBody() as $name => $value) {
                    if (!is_string($value)) {
                        throw ApiError::badRequest();
                    }
                    $fields[(string) $name] = $value;
                }
                $files = [];
                foreach ($request->getUploadedFiles() as $name => $file) {
                    if (!$file instanceof UploadedFileInterface || $file->getError() !== UPLOAD_ERR_OK) {
                        throw ApiError::badRequest();
                    }
                    $files[(string) $name] = $file;
                }
                return [$fields, $files];
            case 'application/json':
                self::decodeJsonObject((string) $request->getBody());
                return [[], []];
            default:
                throw ApiError::unsupportedMediaType();
        }
    }

    /**
     * フォームの数値を解釈する。送られていなければ null
     */
    public static function formNumber(?string $value): ?float
    {
        if ($value === null) {
            return null;
        }
        if ($value === '') {
            return 0.0;
        }
        if (!is_numeric($value) || trim($value) !== $value) {
            throw ApiError::badRequest();
        }
        return (float) $value;
    }

    /**
     * クエリパラメータを取り出す。同じ名前があれば最初の値を使う
     */
    public static function query(ServerRequestInterface $request, string $name): ?string
    {
        return self::queryValues($request, $name)[0] ?? null;
    }

    public static function queryString(ServerRequestInterface $request, string $name): string
    {
        return self::query($request, $name) ?? '';
    }

    /**
     * 時刻のクエリパラメータを取り出す。送られていなければ null
     */
    public static function queryTime(ServerRequestInterface $request, string $name): ?DateTimeImmutable
    {
        $value = self::query($request, $name);
        if ($value === null) {
            return null;
        }
        return Time::parseRfc3339($value) ?? throw ApiError::badRequest();
    }

    public static function queryInt(ServerRequestInterface $request, string $name): ?int
    {
        $value = self::query($request, $name);
        if ($value === null) {
            return null;
        }
        if ($value === '') {
            return 0;
        }
        $number = preg_match('/\A([+-]?)0*([0-9]+)\z/', $value, $m) ? filter_var($m[1] . $m[2], FILTER_VALIDATE_INT) : false;
        return $number === false ? throw ApiError::badRequest() : $number;
    }

    /**
     * クエリパラメータの値をすべて返す。
     * $_GET は同じ名前の値を後勝ちでまとめてしまうので、クエリ文字列を自分で解釈する
     *
     * @return list<string>
     */
    public static function queryValues(ServerRequestInterface $request, string $name): array
    {
        return self::parseQuery(self::rawQuery($request))[$name] ?? [];
    }

    /**
     * 生のクエリ文字列。PSR-7 の URI は不正な % を %25 に直してしまうので、QUERY_STRING を優先する
     */
    public static function rawQuery(ServerRequestInterface $request): string
    {
        $query = $request->getServerParams()['QUERY_STRING'] ?? null;
        return is_string($query) ? $query : $request->getUri()->getQuery();
    }

    /**
     * クエリ文字列を解釈する。解釈できない組は捨てる。
     * $strict が true なら、解釈できない組があったときに null を返す
     *
     * @return array<string, list<string>>|null
     */
    public static function parseQuery(string $query, bool $strict = false): ?array
    {
        $params = [];
        foreach (explode('&', $query) as $pair) {
            if ($pair === '') {
                continue;
            }
            [$key, $value] = array_pad(explode('=', $pair, 2), 2, '');
            $key = self::unescape($key);
            $value = self::unescape($value);
            // Go の url.ParseQuery と同じく ; を含む組は不正とする
            if ($key === null || $value === null || str_contains($pair, ';')) {
                if ($strict) {
                    return null;
                }
                continue;
            }
            $params[$key][] = $value;
        }
        return $params;
    }

    private static function unescape(string $value): ?string
    {
        if (preg_match('/%(?![0-9A-Fa-f]{2})/', $value)) {
            return null;
        }
        return urldecode($value);
    }

    private static function hasBody(ServerRequestInterface $request): bool
    {
        $length = $request->getHeaderLine('Content-Length');
        if ($length !== '') {
            return $length !== '0';
        }
        return $request->hasHeader('Transfer-Encoding') || (string) $request->getBody() !== '';
    }

    /**
     * Content-Type のメディアタイプ。webapp/go（Echo）と同じく大文字と小文字を区別して比べる
     */
    private static function mediaType(ServerRequestInterface $request): string
    {
        return trim(explode(';', $request->getHeaderLine('Content-Type'))[0]);
    }

    /**
     * JSON のオブジェクト（または null）を読む
     *
     * @return array<string, mixed>
     */
    private static function decodeJsonObject(string $body): array
    {
        try {
            $data = json_decode($body, flags: JSON_THROW_ON_ERROR);
        } catch (\JsonException) {
            throw ApiError::badRequest();
        }
        if ($data !== null && !$data instanceof \stdClass) {
            throw ApiError::badRequest();
        }
        return $data === null ? [] : get_object_vars($data);
    }
}
