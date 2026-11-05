// Client-side BFF caller: same discriminated contract as lib/gateway's
// GatewayResult (4xx bodies parsed with their machine codes) but against the
// app's own /api routes with httpOnly-cookie auth. The wizard maps codes to
// per-field/per-step messages instead of crashing. The ok-branch carries the
// route's HTTP status when the call needs it — the convert-to-PO action
// distinguishes 201 (PO draft created) from 200 (idempotent replay, #35);
// optional + backward-compatible for every other caller.
export type ApiResult<T> =
  | { ok: true; data: T; status?: number }
  | { ok: false; status: number; error: string; code: string };

export async function callApi<T>(
  path: string,
  init?: { method?: string; body?: unknown; headers?: Record<string, string> },
): Promise<ApiResult<T>> {
  try {
    const headers: Record<string, string> = { ...(init?.headers ?? {}) };
    let body: string | undefined;
    if (init?.body !== undefined) {
      headers["content-type"] = "application/json";
      body = JSON.stringify(init.body);
    }
    const res = await fetch(path, { method: init?.method ?? "GET", headers, body, cache: "no-store" });
    if (!res.ok) {
      const parsed = (await res.json().catch(() => null)) as { error?: string; code?: string } | null;
      return {
        ok: false,
        status: res.status,
        error: parsed?.error ?? `request failed (${res.status})`,
        code: parsed?.code ?? "gateway_error",
      };
    }
    return { ok: true, data: (await res.json()) as T, status: res.status };
  } catch {
    return { ok: false, status: 502, error: "network error", code: "gateway_unavailable" };
  }
}
