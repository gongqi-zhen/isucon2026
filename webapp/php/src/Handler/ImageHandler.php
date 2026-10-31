<?php

declare(strict_types=1);

namespace Isureal\Handler;

use Isureal\Bytea;
use Isureal\Database;
use Isureal\Http\ApiError;
use Isureal\Http\ActionLogEntry;
use Isureal\Http\ErrorHandler;
use Isureal\Http\Input;
use Isureal\ImageSignature;
use Psr\Http\Message\ResponseInterface as Response;
use Psr\Http\Message\ServerRequestInterface as Request;

final readonly class ImageHandler
{
    public function __construct(private Database $db)
    {
    }

    /**
     * GET /api/images/{imageId}
     *
     * @param array<string, string> $args
     */
    public function show(Request $request, Response $response, array $args): Response
    {
        try {
            $response = $this->image($request, $response, $args['imageId']);
        } catch (\Throwable $e) {
            $response = ErrorHandler::render($response, $e);
        }
        return $response->withHeader('Cache-Control', 'no-store');
    }

    private function image(Request $request, Response $response, string $imageId): Response
    {
        $query = Input::parseQuery(Input::rawQuery($request), strict: true);
        if ($query === null || count($query['exp'] ?? []) !== 1 || count($query['sig'] ?? []) !== 1) {
            throw self::forbidden();
        }
        [$exp, $sig] = [$query['exp'][0], $query['sig'][0]];
        // int64 の範囲の正規化された 10 進数だけを受け付ける（負の値は期限切れとして扱う）
        $expiresAt = filter_var($exp, FILTER_VALIDATE_INT);
        if ($expiresAt === false || (string) $expiresAt !== $exp || $expiresAt <= time()) {
            throw self::forbidden();
        }
        $signature = ImageSignature::base64UrlDecode($sig);
        if ($signature === null || strlen($signature) !== 32) {
            throw self::forbidden();
        }

        $secretKeyHash = $this->db->fetchValue(
            "SELECT u.secret_key_hash
             FROM image_objects i JOIN users u ON u.id = i.owner_user_id
             WHERE i.id = :id AND u.status = 'active'",
            ['id' => $imageId],
        ) ?? throw self::forbidden();
        $key = ImageSignature::signingKey($secretKeyHash);
        if (!hash_equals(ImageSignature::sign($key, $imageId, $expiresAt), $signature)) {
            ActionLogEntry::of($request)->abuse = 'invalid_signature';
            throw self::forbidden();
        }

        // 削除状態を毎回確認する。即時反映も5秒以内の許容範囲に含まれる。
        $object = $this->db->fetchOne(
            "SELECT i.data, i.media_type
             FROM image_objects i JOIN users u ON u.id = i.owner_user_id AND u.status = 'active'
             WHERE i.id = :id AND (
               u.profile_image_id = i.id
               OR EXISTS (
                 SELECT 1 FROM posts p
                 WHERE p.image_id = i.id AND p.author_id = u.id AND p.deleted_at IS NULL
               )
               OR EXISTS (
                 SELECT 1 FROM realmojis r
                 JOIN posts p ON p.id = r.post_id AND p.deleted_at IS NULL
                 JOIN users author ON author.id = p.author_id AND author.status = 'active'
                 WHERE r.image_id = i.id AND r.reactor_id = u.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL
               )
             )",
            ['id' => $imageId],
        ) ?? throw self::forbidden();
        if ($expiresAt <= time()) {
            throw self::forbidden();
        }
        $response->getBody()->write(Bytea::read($object['data']));
        return $response->withHeader('Content-Type', $object['media_type']);
    }

    private static function forbidden(): ApiError
    {
        return new ApiError(403, 'image_forbidden');
    }
}
