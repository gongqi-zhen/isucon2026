export function lookup<T extends object>(table: T, key: PropertyKey): T[keyof T] | undefined {
  return Object.hasOwn(table, key) ? table[key as keyof T] : undefined;
}
