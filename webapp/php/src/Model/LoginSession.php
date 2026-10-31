<?php

declare(strict_types=1);

namespace Isureal\Model;

use DateTimeImmutable;

final readonly class LoginSession
{
    /**
     * @param string $region セッションのユーザーのリージョン
     * @param string $token 作ったときだけ持つ
     */
    public function __construct(
        public string $userId,
        public string $tokenHash,
        public DateTimeImmutable $createdAt,
        public string $region = '',
        public string $token = '',
    ) {
    }
}
