<?php

declare(strict_types=1);

namespace Isureal\Model;

final readonly class PostLocation implements \JsonSerializable
{
    public function __construct(
        public string $kind,
        public ?float $latitude = null,
        public ?float $longitude = null,
        public ?string $locality = null,
    ) {
    }

    public function isValid(): bool
    {
        return match ($this->kind) {
            'none' => $this->latitude === null && $this->longitude === null && $this->locality === null,
            'exact' => $this->latitude !== null && $this->longitude !== null && $this->locality === null
                && $this->latitude >= -90 && $this->latitude <= 90 && $this->longitude >= -180 && $this->longitude <= 180,
            'approximate' => $this->latitude === null && $this->longitude === null && $this->locality !== null
                && User::validText($this->locality, 1, 100),
            default => false,
        };
    }

    /**
     * @return array<string, string|float>
     */
    public function jsonSerialize(): array
    {
        return array_filter([
            'kind' => $this->kind,
            'latitude' => $this->latitude,
            'longitude' => $this->longitude,
            'locality' => $this->locality,
        ], static fn (string|float|null $value): bool => $value !== null);
    }
}
