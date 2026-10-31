<?php

declare(strict_types=1);

namespace Isureal\Repository;

use Isureal\Database;
use Isureal\Model\User;

final readonly class UserRepository
{
    public function __construct(private Database $db)
    {
    }

    /**
     * 退会していないユーザーを返す
     */
    public function find(string $userId): ?User
    {
        $row = $this->db->fetchOne('SELECT ' . User::columns() . " FROM users WHERE id = :id AND status = 'active'", ['id' => $userId]);
        return $row === null ? null : User::fromRow($row);
    }

    /**
     * ユーザーの行をロックする。ユーザーがいなければ false
     */
    public function lock(string $userId, bool $activeOnly = false): bool
    {
        $sql = 'SELECT id FROM users WHERE id = :id' . ($activeOnly ? " AND status = 'active'" : '') . ' FOR NO KEY UPDATE';
        return $this->db->fetchValue($sql, ['id' => $userId]) !== null;
    }

    /**
     * ユーザーの行をロックする。ユーザーがいなければ例外
     */
    public function mustLock(string $userId, bool $activeOnly = false): void
    {
        if (!$this->lock($userId, $activeOnly)) {
            throw new \RuntimeException('failed to lock user: sql: no rows in result set');
        }
    }

    public function mustFind(string $userId): User
    {
        return $this->find($userId) ?? throw new \RuntimeException('sql: no rows in result set');
    }
}
