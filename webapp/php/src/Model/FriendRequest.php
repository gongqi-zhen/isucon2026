<?php

declare(strict_types=1);

namespace Isureal\Model;

use DateTimeImmutable;
use Isureal\Time;

final readonly class FriendRequest
{
    public const string COLUMNS = 'id, requester_id, recipient_id, status, created_at, updated_at';

    public function __construct(
        public string $id,
        public string $requesterId,
        public string $recipientId,
        public string $status,
        public DateTimeImmutable $createdAt,
        public DateTimeImmutable $updatedAt,
    ) {
    }

    /**
     * @param array<string, mixed> $row
     */
    public static function fromRow(array $row): self
    {
        return new self(
            id: $row['id'],
            requesterId: $row['requester_id'],
            recipientId: $row['recipient_id'],
            status: $row['status'],
            createdAt: Time::fromDb($row['created_at']),
            updatedAt: Time::fromDb($row['updated_at']),
        );
    }

    /**
     * @return array<string, string>
     */
    public function toResponse(): array
    {
        return [
            'relationId' => $this->id,
            'status' => $this->status,
            'updatedAt' => Time::format($this->updatedAt),
        ];
    }
}
