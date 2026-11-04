// Timeline wiring tests (issue #43): the acceptance criteria are mostly glue
// this component owns — newest-first rendering with relative timestamps,
// client-side type filtering over loaded events, keyset "Load more", empty /
// skeleton / error states, the 401 refresh dance, and FR-11's 2 s polling
// (asserted with vitest fake timers: /api/events must be fetched again
// exactly when the clock advances 2000 ms). The BFF fetch and
// next/navigation are mocked; a QueryClientProvider supplies context.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import Timeline, { TIMELINE_POLL_INTERVAL_MS } from "./Timeline";
import type { EventEntry, EventsPage } from "@/lib/gateway";

vi.mock("next/navigation", () => ({
  usePathname: () => "/app/requisitions/r-1",
}));

const copy = {
  title: "Timeline",
  empty: "No events yet.",
  loading: "Loading timeline…",
  sessionExpired: "Session expired — redirecting to sign-in…",
  error: "Failed to load events — the gateway may be unreachable.",
  retry: "Retry",
  loadMore: "Load more",
  loadingMore: "Loading…",
  filterLabel: "Event type",
  filterAll: "All types",
};

function event(overrides: Partial<EventEntry>): EventEntry {
  return {
    event_id: "ev-1",
    tenant_id: "t-1",
    type: "pr.created",
    version: 1,
    occurred_at: new Date(Date.now() - 90_000).toISOString(),
    actor_id: "user-1",
    trace_id: "trace-1",
    entity_type: "requisition",
    entity_id: "r-1",
    payload_json: '{"status":"draft"}',
    ...overrides,
  };
}

// Newest-first fixture: approved (2 min ago) → submitted (3 h ago) → created
// (2 d ago). Relative labels assume locale "en" and Date.now() at build time.
function seedFixture(): EventEntry[] {
  return [
    event({ event_id: "ev-3", type: "pr.approved", occurred_at: new Date(Date.now() - 90_000).toISOString() }),
    event({ event_id: "ev-2", type: "pr.submitted", occurred_at: new Date(Date.now() - 3 * 3_600_000).toISOString() }),
    event({ event_id: "ev-1", type: "pr.created", occurred_at: new Date(Date.now() - 2 * 86_400_000).toISOString() }),
  ];
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

function page(events: EventEntry[], nextCursor = ""): EventsPage {
  return { events, next_cursor: nextCursor };
}

const originalLocation = window.location;

beforeEach(() => {
  // jsdom forbids assigning window.location; swap in a stub with a spy so the
  // 401 refresh dance can be asserted.
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { ...originalLocation, assign: vi.fn() },
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
});

function renderTimeline() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <Timeline entityType="requisition" entityId="r-1" locale="en" copy={copy} limit={20} />
    </QueryClientProvider>,
  );
}

function stubBff(responder: Response | Promise<Response> | ((url: string, count: number) => Response)) {
  let calls = 0;
  const fetchMock = vi.fn(async (url: string) => {
    calls += 1;
    return typeof responder === "function" ? responder(url, calls) : responder;
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

describe("Timeline", () => {
  it("renders events newest-first with relative timestamps and forwards the entity filter", async () => {
    const fetchMock = stubBff(jsonResponse(page(seedFixture())));

    renderTimeline();

    const items = await screen.findAllByTestId("timeline-item");
    expect(items).toHaveLength(3);
    // Order is newest-first: approved, submitted, created.
    expect(items[0].textContent).toContain("pr.approved");
    expect(items[1].textContent).toContain("pr.submitted");
    expect(items[2].textContent).toContain("pr.created");
    // Relative timestamps via Intl.RelativeTimeFormat (90 s → 2 minutes).
    expect(screen.getAllByTestId("timeline-time")[0].textContent).toBe("2 minutes ago");
    expect(fetchMock).toHaveBeenCalledWith("/api/events?entityType=requisition&entityId=r-1&limit=20");
  });

  it("filters the loaded events client-side by type", async () => {
    stubBff(jsonResponse(page(seedFixture())));

    renderTimeline();
    await screen.findAllByTestId("timeline-item");

    fireEvent.change(screen.getByTestId("timeline-filter"), { target: { value: "pr.submitted" } });
    const items = screen.getAllByTestId("timeline-item");
    expect(items).toHaveLength(1);
    expect(items[0].textContent).toContain("pr.submitted");
  });

  it("loads the next keyset page via next_cursor and appends older events", async () => {
    const first = page([event({ event_id: "ev-2", type: "pr.approved" })], "Y3Vyc29y");
    const second = page([event({ event_id: "ev-1", type: "pr.created" })]);
    const fetchMock = stubBff((_url, count) => jsonResponse(count === 1 ? first : second));

    renderTimeline();
    expect(await screen.findAllByTestId("timeline-item")).toHaveLength(1);

    fireEvent.click(screen.getByTestId("timeline-load-more"));

    await waitFor(() => expect(screen.getAllByTestId("timeline-item")).toHaveLength(2));
    const [secondUrl] = fetchMock.mock.calls[1] as [string, RequestInit];
    expect(secondUrl).toBe("/api/events?entityType=requisition&entityId=r-1&limit=20&cursor=Y3Vyc29y");
    // Last page: next_cursor empty → no more button.
    await waitFor(() => expect(screen.queryByTestId("timeline-load-more")).toBeNull());
  });

  it("shows the empty state when the aggregate has no events", async () => {
    stubBff(jsonResponse(page([])));

    renderTimeline();

    expect(await screen.findByTestId("timeline-empty")).toBeTruthy();
    expect(screen.queryByTestId("timeline-item")).toBeNull();
  });

  it("shows a skeleton while the first page loads", async () => {
    stubBff(new Promise<Response>(() => {})); // never resolves

    renderTimeline();

    expect(screen.getByTestId("timeline-loading")).toBeTruthy();
    expect(screen.queryByTestId("timeline-item")).toBeNull();
  });

  it("shows the error state with a working retry", async () => {
    const fetchMock = vi.fn(async () => jsonResponse({ error: "boom" }, 500));
    vi.stubGlobal("fetch", fetchMock);

    renderTimeline();

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Failed to load events");
    fireEvent.click(screen.getByTestId("timeline-retry"));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
  });

  it("runs the refresh dance when the BFF answers 401", async () => {
    stubBff(jsonResponse({ error: "Session expired.", code: "unauthorized" }, 401));

    renderTimeline();

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Session expired");
    await waitFor(() =>
      expect(window.location.assign).toHaveBeenCalledWith(
        `/api/auth/refresh?next=${encodeURIComponent("/app/requisitions/r-1")}`,
      ),
    );
  });

  it("polls /api/events again after 2000 ms and not sooner (FR-11)", async () => {
    vi.useFakeTimers();
    const fetchMock = stubBff(jsonResponse(page(seedFixture())));
    expect(TIMELINE_POLL_INTERVAL_MS).toBeLessThanOrEqual(2000);

    renderTimeline();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // 1999 ms: within the interval, no refetch yet.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1999);
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // One more ms crosses the 2000 ms budget: /api/events is fetched again.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[1][0]).toBe("/api/events?entityType=requisition&entityId=r-1&limit=20");
  });
});
