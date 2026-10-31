<?php

declare(strict_types=1);

namespace Isureal;

use PDO;
use PDOStatement;
use Pdo\Pgsql;

/**
 * PDO の薄いラッパー。プレースホルダは名前付き（:name）で書く。
 */
final class Database
{
    public function __construct(private readonly Pgsql $pdo)
    {
    }

    /**
     * @param bool $persistent php-fpm の worker ごとに接続を使い回すかどうか
     */
    public static function connect(Config $config, bool $persistent = true): self
    {
        $pdo = new Pgsql($config->databaseDsn(), $config->dbUser, $config->dbPassword, [
            PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
            PDO::ATTR_DEFAULT_FETCH_MODE => PDO::FETCH_ASSOC,
            PDO::ATTR_PERSISTENT => $persistent,
            // prepare と実行を分けると DB との往復が 2 回になるので、1 回で済ませる
            Pgsql::ATTR_DISABLE_PREPARES => true,
        ]);
        // 前のリクエストが途中で終わってトランザクションが残っていれば捨てる
        if ($pdo->inTransaction()) {
            $pdo->rollBack();
        }
        return new self($pdo);
    }

    /**
     * @param array<string, mixed> $params
     * @return array<string, mixed>|null
     */
    public function fetchOne(string $sql, array $params = []): ?array
    {
        $row = $this->run($sql, $params)->fetch();
        return $row === false ? null : $row;
    }

    /**
     * @param array<string, mixed> $params
     * @return list<array<string, mixed>>
     */
    public function fetchAll(string $sql, array $params = []): array
    {
        /** @var list<array<string, mixed>> */
        return $this->run($sql, $params)->fetchAll();
    }

    /**
     * 先頭の行の先頭の列を返す。行がなければ null
     *
     * @param array<string, mixed> $params
     */
    public function fetchValue(string $sql, array $params = []): mixed
    {
        // fetchColumn() は列の値の false と行がないことを区別できないので、行で受け取る
        $row = $this->run($sql, $params)->fetch(PDO::FETCH_NUM);
        return $row === false ? null : $row[0];
    }

    /**
     * 各行の先頭の列を返す
     *
     * @param array<string, mixed> $params
     * @return list<mixed>
     */
    public function fetchColumn(string $sql, array $params = []): array
    {
        /** @var list<mixed> */
        return $this->run($sql, $params)->fetchAll(PDO::FETCH_COLUMN);
    }

    /**
     * @param array<string, mixed> $params
     * @return int 変更した行数
     */
    public function execute(string $sql, array $params = []): int
    {
        return $this->run($sql, $params)->rowCount();
    }

    /**
     * 複数の文をまとめて実行する（スキーマの作成など）
     */
    public function executeScript(string $sql): void
    {
        $this->pdo->exec($sql);
    }

    /**
     * $callback をトランザクションの中で実行する。例外が出たらロールバックする
     *
     * @template T
     * @param callable(self): T $callback
     * @return T
     */
    public function transaction(callable $callback): mixed
    {
        $this->pdo->beginTransaction();
        try {
            $result = $callback($this);
            $this->pdo->commit();
            return $result;
        } catch (\Throwable $e) {
            if ($this->pdo->inTransaction()) {
                $this->pdo->rollBack();
            }
            throw $e;
        }
    }

    /**
     * IN 句のプレースホルダと値を作る。
     * 例: inClause('id', ['a', 'b']) は [':id0, :id1', ['id0' => 'a', 'id1' => 'b']]
     *
     * @param list<string> $values
     * @return array{string, array<string, string>}
     */
    public static function inClause(string $prefix, array $values): array
    {
        $params = [];
        foreach ($values as $i => $value) {
            $params["{$prefix}{$i}"] = $value;
        }
        $placeholders = implode(', ', array_map(static fn (string $name): string => ":{$name}", array_keys($params)));
        return [$placeholders, $params];
    }

    /**
     * @param array<string, mixed> $params
     */
    private function run(string $sql, array $params): PDOStatement
    {
        $statement = $this->pdo->prepare($sql);
        foreach ($params as $name => $value) {
            $statement->bindValue($name, ...match (true) {
                $value instanceof Bytea => [$value->data, PDO::PARAM_LOB],
                $value === null => [null, PDO::PARAM_NULL],
                is_int($value) => [$value, PDO::PARAM_INT],
                is_bool($value) => [$value, PDO::PARAM_BOOL],
                default => [(string) $value, PDO::PARAM_STR],
            });
        }
        $statement->execute();
        return $statement;
    }
}
