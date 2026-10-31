<?php

declare(strict_types=1);

namespace Isureal\Repository;

use Isureal\Database;
use Isureal\Ids;
use Isureal\Model\AdminSession;
use Isureal\Time;

final readonly class AdminSessionRepository
{
    public function __construct(private Database $db)
    {
    }

    public function findByToken(string $token): ?AdminSession
    {
        $row = $this->db->fetchOne(
            'SELECT administrator_id, token_hash, created_at
             FROM administrator_sessions
             WHERE token_hash = :token_hash AND active = true',
            ['token_hash' => Ids::sha256($token)],
        );
        return $row === null ? null : new AdminSession($row['administrator_id'], $row['token_hash'], Time::fromDb($row['created_at']));
    }

    public function create(string $administratorId): AdminSession
    {
        return $this->db->transaction(function (Database $db) use ($administratorId): AdminSession {
            [$sessionId, $token] = [Ids::generate(), Ids::generate()];
            $db->execute(
                'INSERT INTO administrator_sessions (id, administrator_id, token_hash) VALUES (:id, :administrator_id, :token_hash)',
                ['id' => $sessionId, 'administrator_id' => $administratorId, 'token_hash' => Ids::sha256($token)],
            );
            $row = $db->fetchOne('SELECT administrator_id, token_hash, created_at FROM administrator_sessions WHERE id = :id', ['id' => $sessionId])
                ?? throw new \RuntimeException('failed to get admin session: sql: no rows in result set');
            return new AdminSession($row['administrator_id'], $row['token_hash'], Time::fromDb($row['created_at']), $token);
        });
    }

    public function invalidate(string $administratorId, string $tokenHash): void
    {
        $this->db->execute(
            'UPDATE administrator_sessions
             SET active = false, invalidated_at = clock_timestamp()
             WHERE administrator_id = :administrator_id AND token_hash = :token_hash AND active = true',
            ['administrator_id' => $administratorId, 'token_hash' => $tokenHash],
        );
    }
}
