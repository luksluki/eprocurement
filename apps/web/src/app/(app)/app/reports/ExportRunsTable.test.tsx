// ExportRunsTable wiring tests (issue #49): the run history renders with
// the missed badge and the failed run's error message, the download button
// appears only for successful runs, and clicking it navigates the browser
// to the presigned URL from the BFF (bytes never stream through the app).
// next/navigation and the BFF fetch are mocked; a QueryClientProvider
// wrapper supplies the TanStack Query context (the RequisitionsTable
// pattern).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import ExportRunsTable, { type ExportRunsTableCopy } from "./ExportRunsTable";
import type { ExportRunEntry, ExportRunsPage } from "@/lib/gateway";

const nav = vi.hoisted(() => ({
  pathname: "/app/reports",
}));

vi.mock("next/navigation", () => ({
  usePathname: () => nav.pathname,
}));

const originalLocation = window.location;

const copy: ExportRunsTableCopy = {
  loading: "Loading run history…",
  error: "Failed to load run history — the gateway may be unreachable.",
  retry: "Retry",
  sessionExpired: "Session expired — redirecting to sign-in…",
  empty: "No export runs yet.",
  download: "Download CSV",
  downloading: "Preparing…",
  downloadFailed: "Failed to prepare the download link.",
  trigger: "Run export now",
  triggering: "Queuing…",
  triggerFailed: "Failed to queue the export — try again.",
  manualBadge: "manual",
};

function run(overrides: Partial<ExportRunEntry>): ExportRunEntry {
  return {
    run_id: "run-1",
    tenant_id: "t-1",
    scheduled_for: "2026-10-28T01:00:00Z",
    status: "success",
    trigger: "scheduled",
    row_count: 7,
    missed_previous: false,
    error_message: "",
    started_at: "2026-10-28T01:00:00Z",
    finished_at: "2026-10-28T01:00:02Z",
    ...overrides,
  };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function renderTable(canTrigger = false) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ExportRunsTable copy={copy} canTrigger={canTrigger} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  // jsdom forbids assigning window.location; swap in a stub with a spy so
  // the download navigation can be asserted (the Timeline pattern).
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { ...originalLocation, assign: vi.fn() },
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
});

describe("ExportRunsTable", () => {
  it("renders the run history: statuses, missed badge, error message, download buttons", async () => {
    const page: ExportRunsPage = {
      runs: [
        run({ run_id: "a", status: "success", row_count: 7 }),
        run({
          run_id: "b",
          status: "failed",
          missed_previous: true,
          error_message: "object store unreachable",
          row_count: 0,
        }),
      ],
    };
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse(page)));

    renderTable();

    await waitFor(() => expect(screen.getByTestId("run-a")).toBeTruthy());
    expect(screen.getByTestId("run-b")).toBeTruthy();
    expect(screen.getAllByTestId("run-status").map((el) => el.textContent)).toEqual(["success", "failed"]);
    expect(screen.getByTestId("run-missed").textContent).toBe("missed");
    expect(screen.getByTestId("run-error").textContent).toBe("object store unreachable");
    // Download button only on the successful run.
    expect(screen.getByTestId("run-download")).toBeTruthy();
    expect(screen.queryByTestId("run-download-error")).toBeNull();
  });

  it("navigates the browser to the presigned URL when Download CSV is clicked", async () => {
    const page: ExportRunsPage = { runs: [run({ run_id: "a" })] };
    const url = "http://minio:9000/exports/t-1/2026-10-28/purchase-orders.csv?X-Amz-Signature=sig";
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(page)) // history
      .mockResolvedValueOnce(jsonResponse({ ...page.runs[0], download: { url, expires_at: "x" } })); // download-url
    vi.stubGlobal("fetch", fetchMock);

    renderTable();
    await waitFor(() => expect(screen.getByTestId("run-download")).toBeTruthy());

    fireEvent.click(screen.getByTestId("run-download"));

    await waitFor(() => expect(window.location.assign).toHaveBeenCalledWith(url));
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[1][0]).toBe("/api/exports/runs/a/download-url");
  });

  it("shows the per-row failure state when the download URL cannot be minted", async () => {
    const page: ExportRunsPage = { runs: [run({ run_id: "a" })] };
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(page))
      .mockResolvedValueOnce(jsonResponse({ error: "export run has no file", code: "export_not_succeeded" }, 412));
    vi.stubGlobal("fetch", fetchMock);

    renderTable();
    await waitFor(() => expect(screen.getByTestId("run-download")).toBeTruthy());

    fireEvent.click(screen.getByTestId("run-download"));

    await waitFor(() => expect(screen.getByTestId("run-download-error").textContent).toBe(copy.downloadFailed));
    expect(window.location.assign).not.toHaveBeenCalled();
  });

  it("renders the empty state when no runs exist yet", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ runs: [] })));

    renderTable();

    await waitFor(() => expect(screen.getByTestId("state-empty").textContent).toBe(copy.empty));
    expect(screen.queryByTestId("run-download")).toBeNull();
  });

  it("renders the retryable error state when the BFF fetch fails", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({}, 502)));

    renderTable();

    await waitFor(() => expect(screen.getByTestId("state-error")).toBeTruthy());
    expect(screen.getByTestId("state-error").textContent).toContain(copy.error);
    expect(screen.getByTestId("retry").textContent).toBe(copy.retry);
  });

  it("renders the manual badge on manual runs (#53)", async () => {
    const page: ExportRunsPage = {
      runs: [run({ run_id: "m", trigger: "manual" }), run({ run_id: "n", trigger: "scheduled" })],
    };
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse(page)));

    renderTable();

    await waitFor(() => expect(screen.getByTestId("run-m")).toBeTruthy());
    expect(screen.getByTestId("run-manual").textContent).toBe(copy.manualBadge);
    // Scheduled rows carry no manual badge.
    expect(screen.queryByTestId("run-manual")).toBeTruthy(); // the manual row still has it
    expect(within(screen.getByTestId("run-n")).queryByTestId("run-manual")).toBeNull();
  });

  it("renders the trigger button only when canTrigger (admin)", async () => {
    const page: ExportRunsPage = { runs: [run({ run_id: "a" })] };
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse(page)));

    renderTable(false);
    await waitFor(() => expect(screen.getByTestId("run-a")).toBeTruthy());
    expect(screen.queryByTestId("trigger-export")).toBeNull();
    cleanup();

    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse(page)));
    renderTable(true);
    await waitFor(() => expect(screen.getByTestId("trigger-export")).toBeTruthy());
    expect(screen.getByTestId("trigger-export").textContent).toBe(copy.trigger);
  });

  it("queues a manual export on click and refetches the history", async () => {
    const page: ExportRunsPage = { runs: [run({ run_id: "a" })] };
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(page)) // history
      .mockResolvedValueOnce(jsonResponse({ status: "requested", event_id: "ev-53" }, 202)) // trigger
      .mockResolvedValueOnce(
        jsonResponse({ runs: [run({ run_id: "a" }), run({ run_id: "b", trigger: "manual" })] }),
      ); // refetch after invalidation
    vi.stubGlobal("fetch", fetchMock);

    renderTable(true);
    await waitFor(() => expect(screen.getByTestId("trigger-export")).toBeTruthy());

    fireEvent.click(screen.getByTestId("trigger-export"));

    await waitFor(() => expect(screen.getByTestId("run-b")).toBeTruthy());
    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(fetchMock.mock.calls[1][0]).toBe("/api/exports");
    expect((fetchMock.mock.calls[1][1] as RequestInit).method).toBe("POST");
    // The manual run appears in the refreshed history (AC: manual export
    // appears in run history).
    expect(screen.getByTestId("run-manual").textContent).toBe(copy.manualBadge);
  });

  it("shows the trigger failure state when the queue POST fails", async () => {
    const page: ExportRunsPage = { runs: [run({ run_id: "a" })] };
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(page))
      .mockResolvedValueOnce(jsonResponse({ error: "forbidden", code: "permission_denied" }, 403));
    vi.stubGlobal("fetch", fetchMock);

    renderTable(true);
    await waitFor(() => expect(screen.getByTestId("trigger-export")).toBeTruthy());

    fireEvent.click(screen.getByTestId("trigger-export"));

    await waitFor(() => expect(screen.getByTestId("trigger-error").textContent).toBe(copy.triggerFailed));
    expect(window.location.assign).not.toHaveBeenCalled();
  });
});
