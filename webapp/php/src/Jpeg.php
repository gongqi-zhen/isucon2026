<?php

declare(strict_types=1);

namespace Isureal;

final class Jpeg
{
    /**
     * @return array{int, int}|null
     */
    public static function size(string $data): ?array
    {
        ini_set('gd.jpeg_ignore_warning', '0');
        $path = tempnam(sys_get_temp_dir(), 'jpeg') ?: throw new \RuntimeException('failed to create a temporary file');
        try {
            file_put_contents($path, $data);
            $image = @imagecreatefromjpeg($path);
        } finally {
            unlink($path);
        }
        if ($image === false) {
            return null;
        }
        return [imagesx($image), imagesy($image)];
    }
}
