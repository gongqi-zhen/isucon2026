<?php

declare(strict_types=1);

namespace Isureal;

use DateTimeImmutable;
use DateTimeZone;

/**
 * 時刻の変換。DB とは UTC でやり取りする（Config::databaseDsn）
 */
final class Time
{
    private const string RFC3339_PATTERN = '/\A(\d{4})-(\d\d)-(\d\d)T(\d\d):(\d\d):(\d\d)(?:\.(\d+))?(?:(Z)|([+-])(\d\d):(\d\d))\z/';

    public static function now(): DateTimeImmutable
    {
        return new DateTimeImmutable('now', self::utc());
    }

    /**
     * timestamptz の値（例: 2026-10-24 03:00:00.123457+00）を読む
     */
    public static function fromDb(string $value): DateTimeImmutable
    {
        return new DateTimeImmutable($value)->setTimezone(self::utc());
    }

    public static function fromDbNullable(?string $value): ?DateTimeImmutable
    {
        return $value === null ? null : self::fromDb($value);
    }

    public static function toDb(DateTimeImmutable $time): string
    {
        return $time->format('Y-m-d\TH:i:s.uP');
    }

    /**
     * RFC 3339 の文字列にする。小数部は末尾の 0 を除き、0 なら省く
     */
    public static function format(DateTimeImmutable $time): string
    {
        $utc = $time->setTimezone(self::utc());
        $fraction = rtrim($utc->format('u'), '0');
        return $utc->format('Y-m-d\TH:i:s') . ($fraction === '' ? '' : ".{$fraction}") . 'Z';
    }

    public static function formatNullable(?DateTimeImmutable $time): ?string
    {
        return $time === null ? null : self::format($time);
    }

    /**
     * RFC 3339 の文字列を解釈する。解釈できなければ null
     */
    public static function parseRfc3339(string $value): ?DateTimeImmutable
    {
        if (!preg_match(self::RFC3339_PATTERN, $value, $m, PREG_UNMATCHED_AS_NULL)) {
            return null;
        }
        [, $year, $month, $day, $hour, $minute, $second, $fraction, $zulu, $sign, $offsetHour, $offsetMinute] = array_pad($m, 12, null);
        if (!checkdate((int) $month, (int) $day, (int) $year) || $hour > 23 || $minute > 59 || $second > 59) {
            return null;
        }
        if ($zulu === null && ($offsetHour > 23 || $offsetMinute > 59)) {
            return null;
        }
        $micro = substr(str_pad($fraction ?? '', 6, '0'), 0, 6);
        $offset = $zulu === null ? "{$sign}{$offsetHour}:{$offsetMinute}" : '+00:00';
        $time = DateTimeImmutable::createFromFormat('Y-m-d\\TH:i:s.uP', "{$year}-{$month}-{$day}T{$hour}:{$minute}:{$second}.{$micro}{$offset}");
        return $time === false ? null : $time->setTimezone(self::utc());
    }

    /**
     * YYYY-MM-DD を解釈する。解釈できなければ null
     */
    public static function parseDate(string $value): ?DateTimeImmutable
    {
        if (!preg_match('/\A(\d{4})-(\d\d)-(\d\d)\z/', $value, $m) || !checkdate((int) $m[2], (int) $m[3], (int) $m[1])) {
            return null;
        }
        return new DateTimeImmutable("{$value}T00:00:00", self::utc());
    }

    /**
     * $later - $earlier をマイクロ秒で返す
     */
    public static function diffMicroseconds(DateTimeImmutable $later, DateTimeImmutable $earlier): int
    {
        return self::microseconds($later) - self::microseconds($earlier);
    }

    private static function microseconds(DateTimeImmutable $time): int
    {
        return (int) $time->format('U') * 1_000_000 + (int) $time->format('u');
    }

    private static function utc(): DateTimeZone
    {
        static $utc = new DateTimeZone('UTC');
        return $utc;
    }
}
