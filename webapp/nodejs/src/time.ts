export function now(): Temporal.Instant {
  return Temporal.Now.instant().round({ smallestUnit: "microsecond", roundingMode: "trunc" });
}

export function unix(instant: Temporal.Instant): number {
  return Math.floor(instant.epochMilliseconds / 1000);
}

export function parsePostgresTimestamp(text: string): Temporal.Instant {
  return Temporal.Instant.from(text);
}

const rfc3339Pattern = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:[.,](\d+))?(?:(Z)|([+-])(\d{2}):(\d{2}))$/;

export function parseRFC3339(value: string): Temporal.Instant {
  const match = rfc3339Pattern.exec(value);
  if (!match) {
    throw new Error(`parsing time ${JSON.stringify(value)} as "2006-01-02T15:04:05Z07:00"`);
  }
  const [, year, month, day, hour, minute, second, fraction, zulu, sign, offsetHour, offsetMinute] = match;
  try {
    Temporal.PlainDateTime.from(
      { year: Number(year), month: Number(month), day: Number(day), hour: Number(hour), minute: Number(minute), second: Number(second) },
      { overflow: "reject" },
    );
  } catch {
    throw new Error(`parsing time ${JSON.stringify(value)}: value out of range`);
  }
  if (!zulu && (Number(offsetHour) > 23 || Number(offsetMinute) > 59)) {
    throw new Error(`parsing time ${JSON.stringify(value)}: time zone offset out of range`);
  }
  const fractionPart = fraction ? "." + fraction.slice(0, 6) : "";
  const offset = zulu ? "Z" : `${sign}${offsetHour}:${offsetMinute}`;
  return Temporal.Instant.from(`${year}-${month}-${day}T${hour}:${minute}:${second}${fractionPart}${offset}`);
}

export function validDate(year: number, month: number, day: number): boolean {
  try {
    Temporal.PlainDate.from({ year, month, day }, { overflow: "reject" });
    return true;
  } catch {
    return false;
  }
}
