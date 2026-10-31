<?php

declare(strict_types=1);

namespace Isureal\Model;

use DateTimeImmutable;
use Isureal\Time;

final readonly class Comment
{
    public const string COLUMNS = 'c.id, c.post_id, c.author_id, c.body, c.created_at, c.deleted_at';

    public function __construct(
        public string $id,
        public string $postId,
        public string $authorId,
        public string $body,
        public DateTimeImmutable $createdAt,
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
            authorId: $row['author_id'],
            body: $row['body'],
            createdAt: Time::fromDb($row['created_at']),
            deletedAt: Time::fromDbNullable($row['deleted_at']),
        );
    }

    /**
     * @return array<string, mixed>
     */
    public function toResponse(User $author): array
    {
        return [
            'id' => $this->id,
            'author' => $author->toPublic(),
            'body' => $this->body,
            'createdAt' => Time::format($this->createdAt),
        ];
    }
}
