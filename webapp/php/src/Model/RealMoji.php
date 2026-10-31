<?php

declare(strict_types=1);

namespace Isureal\Model;

use DateTimeImmutable;
use Isureal\ImageSignature;
use Isureal\Time;

final readonly class RealMoji
{
    public const string COLUMNS = 'r.id, r.post_id, r.reactor_id, r.image_id, r.reaction_type, r.created_at, r.replaced_at, r.deleted_at';

    public const array REACTION_TYPES = ['like', 'smile', 'surprise', 'heart', 'laugh', 'instant'];

    public function __construct(
        public string $id,
        public string $postId,
        public string $reactorId,
        public string $imageId,
        public string $reactionType,
        public DateTimeImmutable $createdAt,
        public ?DateTimeImmutable $replacedAt,
        public ?DateTimeImmutable $deletedAt,
    ) {
    }

    /**
     * @param array<string, mixed> $row
     */
    public static function fromRow(array $row): self
    {
        return new self(
            id: $row['id'],
            postId: $row['post_id'],
            reactorId: $row['reactor_id'],
            imageId: $row['image_id'],
            reactionType: $row['reaction_type'],
            createdAt: Time::fromDb($row['created_at']),
            replacedAt: Time::fromDbNullable($row['replaced_at']),
            deletedAt: Time::fromDbNullable($row['deleted_at']),
        );
    }

    /**
     * @return array<string, mixed>
     */
    public function toResponse(User $reactor): array
    {
        return [
            'id' => $this->id,
            'reactor' => $reactor->toPublic(),
            'reactionType' => $this->reactionType,
            'imageUrl' => ImageSignature::url($reactor, $this->imageId),
            'createdAt' => Time::format($this->createdAt),
        ];
    }
}
