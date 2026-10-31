<?php

declare(strict_types=1);

namespace Isureal\Http;

use Psr\Http\Message\ServerRequestInterface;

final class ActionLogEntry
{
    public const string ATTRIBUTE = 'action_log';

    public ?string $userId = null;

    public ?string $username = null;

    public ?string $abuse = null;

    public static function of(ServerRequestInterface $request): self
    {
        $entry = $request->getAttribute(self::ATTRIBUTE);
        return $entry instanceof self ? $entry : new self();
    }
}
