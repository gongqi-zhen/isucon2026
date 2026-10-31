<?php

declare(strict_types=1);

namespace Isureal\Model;

use DateTimeImmutable;

final readonly class AdminSession
{
    /**
     * @param string $token 作ったときだけ持つ
     */
    public function __construct(
        public string $administratorId,
        public string $tokenHash,
        public DateTimeImmutable $createdAt,
        public string $token = '',
    ) {
    }
}
