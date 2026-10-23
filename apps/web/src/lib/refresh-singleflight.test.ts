// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { refreshPair, RefreshFailed, resetSingleFlightForTests } from "./refresh-singleflight";

// Identity revokes the whole token family when a rotated refresh token is
// presented again (docs/schema/identity.md). These tests pin the property the
// single-flight module exists for: one gateway call per refresh token, ever,
// within the process window.

const pair = {
  access_token: "at-2",
  access_expires_in: 900,
  refresh_token: "rt-2",
  refresh_expires_in: 604800,
  token_type: "Bearer",
};

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

beforeEach(() => {
  resetSingleFlightForTests();
  process.env.GATEWAY_URL = "http://gateway.test";
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.GATEWAY_URL;
});

describe("refreshPair single-flight", () => {
  it("coalesces two concurrent refreshes with the same token into ONE gateway call", async () => {
    const fetchMock = vi.fn(async () => json(pair));
    vi.stubGlobal("fetch", fetchMock);

    const [a, b] = await Promise.all([refreshPair("rt-1"), refreshPair("rt-1")]);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledWith(
      "http://gateway.test/api/v1/auth/refresh",
      expect.objectContaining({ method: "POST" }),
    );
    expect(a.access_token).toBe("at-2");
    expect(b.refresh_token).toBe("rt-2");
  });

  it("serves a late request still holding the old token from the cache", async () => {
    const fetchMock = vi.fn(async () => json(pair));
    vi.stubGlobal("fetch", fetchMock);

    await refreshPair("rt-1");
    const late = await refreshPair("rt-1"); // old cookie, after rotation

    expect(late.access_token).toBe("at-2");
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("refreshes distinct tokens independently", async () => {
    const fetchMock = vi.fn(async (_input: unknown, init?: RequestInit) => {
      const { refresh_token: presented } = JSON.parse(String(init?.body)) as { refresh_token: string };
      return json({ ...pair, refresh_token: `new-${presented}` });
    });
    vi.stubGlobal("fetch", fetchMock);

    const [a, b] = await Promise.all([refreshPair("rt-a"), refreshPair("rt-b")]);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(a.refresh_token).toBe("new-rt-a");
    expect(b.refresh_token).toBe("new-rt-b");
  });

  it("does not cache failures — a rejected refresh is retryable", async () => {
    const fetchMock = vi.fn(async () => json({ error: "invalid refresh token" }, 401));
    vi.stubGlobal("fetch", fetchMock);

    await expect(refreshPair("rt-1")).rejects.toBeInstanceOf(RefreshFailed);
    await expect(refreshPair("rt-1")).rejects.toThrow("status 401");
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("maps network failures to RefreshFailed(502)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("fetch failed");
      }),
    );
    await expect(refreshPair("rt-1")).rejects.toMatchObject({ status: 502 });
  });
});
