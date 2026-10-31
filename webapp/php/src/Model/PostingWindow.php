<?php

declare(strict_types=1);

namespace Isureal\Model;

use DateTimeImmutable;
use Isureal\Time;

final readonly class PostingWindow
{
    public const string COLUMNS = 'id, region, local_date, starts_at, on_time_deadline, ended_at';

    public function __construct(
        public string $id,
        public string $region,
        public string $localDate,
        public DateTimeImmutable $startsAt,
        public DateTimeImmutable $onTimeDeadline,
        public ?DateTimeImmutable $endedAt,
    ) {
    }

    /**
     * @param array<string, mixed> $row
     */
    public static function fromRow(array $row): self
    {
        return new self(
            id: $row['id'],
            region: $row['region'],
            localDate: $row['local_date'],
            startsAt: Time::fromDb($row['starts_at']),
            onTimeDeadline: Time::fromDb($row['on_time_deadline']),
            endedAt: Time::fromDbNullable($row['ended_at']),
        );
    }

    /**
     * @return array<string, string>
     */
    public function toResponse(): array
    {
        return [
            'id' => $this->id,
            'region' => $this->region,
            'localDate' => $this->localDate,
            'startsAt' => Time::format($this->startsAt),
            'onTimeDeadline' => Time::format($this->onTimeDeadline),
        ];
    }
}
