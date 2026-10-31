<?php

declare(strict_types=1);

namespace Isureal\Repository;

use Isureal\Database;

final readonly class IpBanRepository
{
    public function __construct(private Database $db)
    {
    }

    public function isBanned(string $ip): bool
    {
        return (bool) $this->db->fetchValue(
            'SELECT EXISTS(SELECT 1 FROM ip_bans WHERE ip = :ip AND banned_until > clock_timestamp())',
            ['ip' => $ip],
        );
    }
}
