<?php

declare(strict_types=1);

namespace Isureal\Repository;

use Isureal\Database;
use Isureal\Ids;
use Isureal\Model\LoginSession;
use Isureal\Time;

final readonly class SessionRepository
{
    public function __construct(private Database $db, private UserRepository $users)
    {
    }

    /**
     * 有効なセッションをトークンから探す
     */
    public function findByToken(string $token): ?LoginSession
    {
        $row = $this->db->fetchOne(
            "SELECT s.user_id, s.token_hash, s.created_at, u.region
             FROM login_sessions s JOIN users u ON u.id = s.user_id
             WHERE s.token_hash = :token_hash
               AND s.active = true AND u.status = 'active'",
            ['token_hash' => Ids::sha256($token)],
        );
        if ($row === null) {
            return null;
        }
        return new LoginSession($row['user_id'], $row['token_hash'], Time::fromDb($row['created_at']), $row['region']);
    }

    // 2026-09-03: リファクタリング後にこの関数の利用がないようです。消すのはいかがでしょうか
    public function createInTransaction(string $userId): LoginSession
    {
        return $this->db->transaction(fn (): LoginSession => $this->create($userId));
    }

    /**
     * ログインセッションを作り、ユーザーの他のセッションを無効にする。トランザクションの中で呼ぶ
     */
    public function create(string $userId): LoginSession
    {
        $token = Ids::generate();
        $this->users->mustLock($userId, activeOnly: true);
        $this->db->execute(
            "UPDATE login_sessions
             SET active = false, invalidated_at = clock_timestamp(), reason = 'replaced'
             WHERE user_id = :user_id AND active = true",
            ['user_id' => $userId],
        );
        $sessionId = Ids::generate();
        $this->db->execute(
            'INSERT INTO login_sessions (id, user_id, token_hash) VALUES (:id, :user_id, :token_hash)',
            ['id' => $sessionId, 'user_id' => $userId, 'token_hash' => Ids::sha256($token)],
        );
        $row = $this->db->fetchOne('SELECT user_id, token_hash, created_at FROM login_sessions WHERE id = :id', ['id' => $sessionId])
            ?? throw new \RuntimeException('failed to get session: sql: no rows in result set');
        return new LoginSession($row['user_id'], $row['token_hash'], Time::fromDb($row['created_at']), token: $token);
    }

    public function invalidate(string $userId, string $tokenHash): void
    {
        $this->db->transaction(function (Database $db) use ($userId, $tokenHash): void {
            $this->users->mustLock($userId);
            $db->execute(
                "UPDATE login_sessions
                 SET active = false, invalidated_at = clock_timestamp(), reason = 'logout'
                 WHERE user_id = :user_id AND active = true AND token_hash = :token_hash",
                ['user_id' => $userId, 'token_hash' => $tokenHash],
            );
        });
    }
}
