<?php

declare(strict_types=1);

namespace Isureal\Model;

use DateTimeImmutable;
use Isureal\ImageSignature;
use Isureal\Time;

final readonly class Post
{
    /**
     * 投稿と投稿期間を JOIN して取り出す列（posts p, posting_windows w）
     */
    public const string COLUMNS = <<<'SQL'
        p.id, p.author_id, p.posting_window_id, p.image_id, p.caption, p.visibility,
          p.location_kind, p.latitude, p.longitude, p.locality, p.posted_at,
          w.on_time_deadline, w.ended_at AS window_ended_at
        SQL;

    public function __construct(
        public string $id,
        public string $authorId,
        public string $postingWindowId,
        public string $imageId,
        public string $caption,
        public string $visibility,
        public PostLocation $location,
        public DateTimeImmutable $postedAt,
        public DateTimeImmutable $onTimeDeadline,
        public ?DateTimeImmutable $windowEndedAt,
    ) {
    }

    /**
     * @param array<string, mixed> $row
     */
    public static function fromRow(array $row): self
    {
        return new self(
            id: $row['id'],
            authorId: $row['author_id'],
            postingWindowId: $row['posting_window_id'],
            imageId: $row['image_id'],
            caption: $row['caption'],
            visibility: $row['visibility'],
            location: new PostLocation(
                kind: $row['location_kind'],
                latitude: $row['latitude'] === null ? null : (float) $row['latitude'],
                longitude: $row['longitude'] === null ? null : (float) $row['longitude'],
                locality: $row['locality'],
            ),
            postedAt: Time::fromDb($row['posted_at']),
            onTimeDeadline: Time::fromDb($row['on_time_deadline']),
            windowEndedAt: Time::fromDbNullable($row['window_ended_at']),
        );
    }

    /**
     * @return array<string, mixed>
     */
    public function toResponse(User $author): array
    {
        $onTime = $this->postedAt <= $this->onTimeDeadline;
        return [
            'id' => $this->id,
            'author' => $author->toPublic(),
            'postingWindowId' => $this->postingWindowId,
            'imageUrl' => ImageSignature::url($author, $this->imageId),
            'caption' => $this->caption,
            'visibility' => $this->visibility,
            'location' => $this->location,
            'postedAt' => Time::format($this->postedAt),
            'onTime' => $onTime,
            'lateSeconds' => $onTime ? 0 : intdiv(Time::diffMicroseconds($this->postedAt, $this->onTimeDeadline), 1_000_000),
        ];
    }
}
