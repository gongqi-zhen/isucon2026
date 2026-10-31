<?php

declare(strict_types=1);

namespace Isureal\Model;

enum Region: string
{
    case America = 'america';
    case Europe = 'europe';
    case Asia = 'asia';

    public static function isValid(string $region): bool
    {
        return self::tryFrom($region) !== null;
    }
}
