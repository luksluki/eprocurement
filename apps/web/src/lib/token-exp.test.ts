import { describe, expect, it } from "vitest";
import { decodeJWTExp, tokenNeedsRefresh } from "./token-exp";

// Mint an unsigned JWT-shaped token: header.payload.sig with base64url
// segments — enough for the local expiry check, which never verifies.
function jwt(payload: Record<string, unknown>): string {
  const b64url = (value: unknown) =>
    btoa(JSON.stringify(value)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  return `${b64url({ alg: "EdDSA", typ: "JWT" })}.${b64url(payload)}.signature`;
}

const now = () => Math.floor(Date.now() / 1000);

describe("decodeJWTExp", () => {
  it("reads exp from the payload without verification", () => {
    expect(decodeJWTExp(jwt({ sub: "u1", exp: 1900000000 }))).toBe(1900000000);
  });

  it("returns null for missing exp, malformed JSON, or wrong shape", () => {
    expect(decodeJWTExp(jwt({ sub: "u1" }))).toBeNull();
    expect(decodeJWTExp("garbage")).toBeNull();
    expect(decodeJWTExp("only.two")).toBeNull();
    expect(decodeJWTExp(`e30.e30.sig`)).toBeNull(); // empty payloads
  });
});

describe("tokenNeedsRefresh", () => {
  it("flags expired tokens", () => {
    expect(tokenNeedsRefresh(jwt({ exp: now() - 10 }))).toBe(true);
  });

  it("flags tokens inside the 30 s skew window", () => {
    expect(tokenNeedsRefresh(jwt({ exp: now() + 10 }))).toBe(true);
    expect(tokenNeedsRefresh(jwt({ exp: now() + 60 }))).toBe(false);
  });

  it("flags malformed tokens (cannot tell → refresh)", () => {
    expect(tokenNeedsRefresh("garbage")).toBe(true);
    expect(tokenNeedsRefresh("a.b.c.d")).toBe(true);
  });
});
