<?php

declare(strict_types=1);

namespace Isureal;

use Isureal\Model\User;

/**
 * 画像の署名付き URL
 */
final class ImageSignature
{
    public const int URL_LIFETIME = 24 * 60 * 60;
    private const int ITERATIONS = 1000;

    /**
     * secret_key_hash（16 進数の SHA-256）を署名の鍵にする
     */
    public static function signingKey(string $secretKeyHash): string
    {
        if (!preg_match('/\A[0-9a-fA-F]{64}\z/', $secretKeyHash)) {
            throw new \UnexpectedValueException('invalid image signing key');
        }
        return (string) hex2bin($secretKeyHash);
    }

    public static function sign(string $key, string $imageId, int $exp): string
    {
        $value = "{$exp}:{$imageId}";
        for ($i = 0; $i < self::ITERATIONS; $i++) {
            $value = hash_hmac('sha256', $value, $key, true);
        }
        return $value;
    }

    public static function url(User $user, string $imageId): string
    {
        $exp = time() + self::URL_LIFETIME;
        return '/api/images/' . rawurlencode($imageId) . '?' . http_build_query([
            'exp' => $exp,
            'sig' => self::base64UrlEncode(self::sign($user->imageKey(), $imageId, $exp)),
        ]);
    }

    public static function base64UrlEncode(string $data): string
    {
        return rtrim(strtr(base64_encode($data), '+/', '-_'), '=');
    }

    /**
     * パディングなしの base64url を厳密に読む。読めなければ null
     */
    public static function base64UrlDecode(string $value): ?string
    {
        if (!preg_match('/\A[A-Za-z0-9_-]*\z/', $value)) {
            return null;
        }
        $data = base64_decode(strtr($value, '-_', '+/'), true);
        return $data === false || self::base64UrlEncode($data) !== $value ? null : $data;
    }
}
