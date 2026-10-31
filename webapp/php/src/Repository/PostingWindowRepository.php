<?php

declare(strict_types=1);

namespace Isureal\Repository;

use Isureal\Database;
use Isureal\Model\PostingWindow;

final readonly class PostingWindowRepository
{
    public function __construct(private Database $db)
    {
    }

    public function find(string $windowId): ?PostingWindow
    {
        $row = $this->db->fetchOne('SELECT ' . PostingWindow::COLUMNS . ' FROM posting_windows WHERE id = :id', ['id' => $windowId]);
        return $row === null ? null : PostingWindow::fromRow($row);
    }

    public function mustFind(string $windowId): PostingWindow
    {
        return $this->find($windowId) ?? throw new \RuntimeException('sql: no rows in result set');
    }
}
