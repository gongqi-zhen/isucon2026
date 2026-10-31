<?php

declare(strict_types=1);

namespace Isureal\Model;

use Isureal\ImageSignature;

final class User
{
    public const array COLUMNS = ['id', 'username', 'secret_key_hash', 'display_name', 'bio', 'profile_image_id', 'region', 'account_type', 'status'];

    private ?string $imageKey = null;

    public function __construct(
        public readonly string $id,
        public readonly string $username,
        public readonly string $secretKeyHash,
        public readonly string $displayName,
        public string $bio,
        public readonly ?string $profileImageId,
        public readonly string $region,
        public readonly string $accountType,
        public readonly string $status,
    ) {
    }

    /**
     * @param array<string, mixed> $row
     * @param string $prefix 列名の接頭辞（columnsAs で付けたもの）
     */
    public static function fromRow(array $row, string $prefix = ''): self
    {
        return new self(
            id: $row["{$prefix}id"],
            username: $row["{$prefix}username"],
            secretKeyHash: $row["{$prefix}secret_key_hash"],
            displayName: $row["{$prefix}display_name"],
            bio: $row["{$prefix}bio"],
            profileImageId: $row["{$prefix}profile_image_id"],
            region: $row["{$prefix}region"],
            accountType: $row["{$prefix}account_type"],
            status: $row["{$prefix}status"],
        );
    }

    public static function columns(): string
    {
        return implode(', ', self::COLUMNS);
    }

    /**
     * 例: columnsAs('u', 'author') は u.id AS "author.id", ...
     */
    public static function columnsAs(string $alias, string $prefix): string
    {
        return implode(', ', array_map(static fn (string $column): string => "{$alias}.{$column} AS \"{$prefix}.{$column}\"", self::COLUMNS));
    }

    public static function validUsername(string $username): bool
    {
        return preg_match('/\A[a-z0-9_.-]{1,64}\z/', $username) === 1;
    }

    public static function validText(string $value, int $minimum, int $maximum): bool
    {
        if (!mb_check_encoding($value, 'UTF-8')) {
            return false;
        }
        $length = mb_strlen($value, 'UTF-8');
        return $length >= $minimum && $length <= $maximum;
    }

    public function isCertified(): bool
    {
        return $this->accountType === 'certified_public';
    }

    /**
     * 画像の署名の鍵
     */
    public function imageKey(): string
    {
        return $this->imageKey ??= ImageSignature::signingKey($this->secretKeyHash);
    }

    public function profileImageUrl(): ?string
    {
        return $this->profileImageId === null ? null : ImageSignature::url($this, $this->profileImageId);
    }

    /**
     * @return array<string, string>
     */
    public function toSessionUser(): array
    {
        return [
            'id' => $this->id,
            'username' => $this->username,
            'displayName' => $this->displayName,
            'bio' => $this->bio,
            'region' => $this->region,
            'accountType' => $this->accountType,
            'status' => $this->status,
        ];
    }

    /**
     * @return array<string, ?string>
     */
    public function toMe(): array
    {
        return [...$this->toSessionUser(), 'profileImageUrl' => $this->profileImageUrl()];
    }

    /**
     * 他のユーザーから見たときの形
     *
     * @return array<string, ?string>
     */
    public function toPublic(): array
    {
        return [
            'id' => $this->id,
            'username' => $this->username,
            'displayName' => $this->displayName,
            'profileImageUrl' => $this->profileImageUrl(),
            'bio' => $this->bio,
            'accountType' => $this->accountType,
        ];
    }
}
