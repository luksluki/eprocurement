// Local access-token expiry check. decodeJWTExp reads the `exp` claim from
// the JWT payload WITHOUT verifying the signature — this is purely a
// navigation decision ("is this token worth presenting?"); actual
// verification stays at the gateway (ADR-0001).

export function decodeJWTExp(token: string): number | null {
  const parts = token.split(".");
  if (parts.length !== 3) {
    return null;
  }
  try {
    const payload = JSON.parse(new TextDecoder().decode(decodeBase64Url(parts[1]))) as { exp?: unknown };
    return typeof payload.exp === "number" ? payload.exp : null;
  } catch {
    return null;
  }
}

function decodeBase64Url(input: string): Uint8Array {
  const normalized = input.replace(/-/g, "+").replace(/_/g, "/");
  const padded = normalized + "=".repeat((4 - (normalized.length % 4)) % 4);
  const binary = atob(padded);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
}

// tokenNeedsRefresh reports whether the access token is expired or about to
// be (30 s skew), or malformed enough that we cannot tell.
export function tokenNeedsRefresh(token: string, skewSeconds = 30): boolean {
  const exp = decodeJWTExp(token);
  if (exp === null) {
    return true;
  }
  return exp <= Math.floor(Date.now() / 1000) + skewSeconds;
}
