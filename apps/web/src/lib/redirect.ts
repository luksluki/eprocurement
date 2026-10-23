// safeNext validates a `next` redirect parameter as a same-origin absolute
// path. Everything else — other origins ("https://evil.example"),
// protocol-relative ("//evil"), backslash tricks, anything with a scheme
// separator — falls back to the default. This is the open-redirect guard for
// every handler that consumes user-supplied redirect targets.
export function safeNext(value: string | null | undefined, fallback = "/app/dashboard"): string {
  if (!value) {
    return fallback;
  }
  if (!value.startsWith("/")) {
    return fallback;
  }
  if (value.startsWith("//") || value.startsWith("/\\")) {
    return fallback;
  }
  // No ":" anywhere (rejects "/\evil", scheme smuggles) and no backslashes.
  if (value.includes(":") || value.includes("\\")) {
    return fallback;
  }
  return value;
}
