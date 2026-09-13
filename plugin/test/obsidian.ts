/**
 * Runtime stand-in for the `obsidian` module, aliased in by vitest.config.ts.
 *
 * Only what sync.ts actually touches at runtime. Type checking still uses the
 * real obsidian.d.ts, so this cannot drift into pretending an API exists.
 */

export function normalizePath(path: string): string {
  return path
    .replace(/\\/g, "/")
    .replace(/\/{2,}/g, "/")
    .replace(/^\/+|\/+$/g, "")
    .trim();
}

export class Notice {
  constructor(public message: string) {}
}

export const Platform = { isMobile: false };

export class Plugin {}

export function requestUrl(): never {
  throw new Error("requestUrl is not stubbed: the test should inject a fake store");
}
