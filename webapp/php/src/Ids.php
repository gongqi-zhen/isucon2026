<?php

declare(strict_types=1);

namespace Isureal;

use Symfony\Component\Uid\Uuid;

final class Ids
{
    /**
     * ランダムな UUID（v4）を返す。ID とトークンに使う
     */
    public static function generate(): string
    {
        return Uuid::v4()->toRfc4122();
    }

    public static function sha256(string $value): string
    {
        return hash('sha256', $value);
    }

    /**
     * 2 つの ID をバイト列の順に並べる
     *
     * @return array{string, string}
     */
    public static function ordered(string $a, string $b): array
    {
        return strcmp($a, $b) <= 0 ? [$a, $b] : [$b, $a];
    }
}
