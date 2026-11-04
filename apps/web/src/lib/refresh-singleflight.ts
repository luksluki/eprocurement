// Single-flight refresh. Identity's rotation semantics (docs/schema/identity.md)
// revoke the whole token family when a rotated refresh token is presented
// again — so two parallel refreshes holding the same token (two tabs, or the
// layout and a page rendering concurrently) would nuke the session. All
// refreshes therefore funnel through here: one in-process call per refresh
// token, with a short result cache so a late request still holding the old
// token picks up the new pair without touching the gateway.
//
// Scope: in-process state, adequate for the single-instance BFF demo; a
// multi-instance deployment needs a shared store (noted in the dev note).

import { gatewayUrl, type TokenPair } from "./gateway";

const CACHE_TTL_MS = 60_000;

const inflight = new Map<string, Promise<TokenPair>>();
const cache = new Map<string, { pair: TokenPair; expiresAt: number }>();

// RefreshFailed carries the gateway status so the caller can distinguish
// "session gone, sign in again" (401) from "gateway unavailable" (502).
export class RefreshFailed extends Error {
  constructor(public readonly status: number) {
    super(`refresh failed with status ${status}`);
    this.name = "RefreshFailed";
  }
}

export async function refreshPair(refreshToken: string): Promise<TokenPair> {
  const hit = cache.get(refreshToken);
  if (hit && hit.expiresAt > Date.now()) {
    return hit.pair;
  }

  let pending = inflight.get(refreshToken);
  if (!pending) {
    pending = doRefresh(refreshToken).finally(() => {
      inflight.delete(refreshToken);
    });
    inflight.set(refreshToken, pending);
  }
  return pending;
}

async function doRefresh(refreshToken: string): Promise<TokenPair> {
  let res: Response;
  try {
    res = await fetch(`${gatewayUrl()}/api/v1/auth/refresh`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ refresh_token: refreshToken }),
      cache: "no-store",
    });
  } catch {
    throw new RefreshFailed(502);
  }
  if (!res.ok) {
    // Not cached: a failed refresh must be retryable, and reuse-revocation
    // only applies to successful rotations.
    throw new RefreshFailed(res.status);
  }
  const pair = (await res.json()) as TokenPair;
  cache.set(refreshToken, { pair, expiresAt: Date.now() + CACHE_TTL_MS });
  return pair;
}

export function resetSingleFlightForTests(): void {
  inflight.clear();
  cache.clear();
}
