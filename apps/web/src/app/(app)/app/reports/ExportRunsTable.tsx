"use client";

// ExportRunsTable (issues #49 + #53, FR-10): the export run history plus
// the manual trigger. TanStack Query fetches /api/exports/runs; each
// successful row carries a Download CSV button that asks
// /api/exports/runs/{id}/download-url for a short-lived presigned GET and
// then navigates the browser straight to MinIO — the CSV bytes never
// stream through the BFF (the attachment convention). A 401 runs the
// refresh dance; missed_previous renders the warning badge (PRD §14.8
// missed-run detection surfacing in the UI). canTrigger (the page derives
// it from session.claims.roles) renders the admin-only "Run export now"
// button: POST /api/exports queues an export.requested event and
// invalidates ["export-runs"], so the manual run appears in the history
// once the worker records it (trigger badge "manual").
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { usePathname } from "next/navigation";
import type { ExportRunsPage } from "@/lib/gateway";

class UnauthorizedError extends Error {
  constructor() {
    super("unauthorized");
  }
}

async function fetchRuns(): Promise<ExportRunsPage> {
  const res = await fetch("/api/exports/runs");
  if (res.status === 401) {
    throw new UnauthorizedError(); // triggers the refresh dance below
  }
  if (!res.ok) {
    throw new Error(`export runs failed (${res.status})`);
  }
  return (await res.json()) as ExportRunsPage;
}

async function requestExport(): Promise<void> {
  const res = await fetch("/api/exports", { method: "POST" });
  if (res.status === 401) {
    throw new UnauthorizedError();
  }
  if (!res.ok) {
    throw new Error(`export trigger failed (${res.status})`);
  }
}

export type ExportRunsTableCopy = {
  loading: string;
  error: string;
  retry: string;
  sessionExpired: string;
  empty: string;
  download: string;
  downloading: string;
  downloadFailed: string;
  trigger: string;
  triggering: string;
  triggerFailed: string;
  manualBadge: string;
};

export default function ExportRunsTable({
  copy,
  canTrigger = false,
}: {
  copy: ExportRunsTableCopy;
  canTrigger?: boolean;
}) {
  const pathname = usePathname();
  const queryClient = useQueryClient();
  // The manual trigger answers 202 before the worker records the run, so a
  // single invalidation can lose the race (refetchOnWindowFocus is off in
  // Providers). Until the manual-run count grows past its value at trigger
  // time, the history polls every 2 s — capped at 30 s so a down worker
  // never polls forever (the row then lands on the next mount/refetch).
  const [polling, setPolling] = useState(false);
  const manualRunsAtTrigger = useRef(0);
  const { data, isPending, isError, error, refetch } = useQuery({
    queryKey: ["export-runs"],
    queryFn: fetchRuns,
    refetchInterval: polling ? 2000 : false,
  });
  const manualRunCount = (data?.runs ?? []).filter((r) => r.trigger === "manual").length;
  useEffect(() => {
    if (!polling) return;
    if (manualRunCount > manualRunsAtTrigger.current) {
      setPolling(false);
      return;
    }
    const stop = setTimeout(() => setPolling(false), 30_000);
    return () => clearTimeout(stop);
  }, [polling, manualRunCount]);
  const [triggerState, setTriggerState] = useState<"idle" | "busy" | "failed">("idle");

  // A 401 from the BFF means the access token expired: run the refresh
  // dance (browser navigation that rotates the cookies and returns here).
  const unauthorized = error instanceof UnauthorizedError;
  useEffect(() => {
    if (unauthorized) {
      window.location.assign(`/api/auth/refresh?next=${encodeURIComponent(pathname)}`);
    }
  }, [unauthorized, pathname]);

  async function trigger() {
    setTriggerState("busy");
    try {
      manualRunsAtTrigger.current = (
        queryClient.getQueryData<ExportRunsPage>(["export-runs"])?.runs ?? []
      ).filter((r) => r.trigger === "manual").length;
      await requestExport();
      // The run is queued; the worker records it shortly — the immediate
      // invalidation races that write, so polling (above) keeps the history
      // fresh until the manual row actually lands (AC: manual export
      // appears in run history).
      setTriggerState("idle");
      setPolling(true);
      await queryClient.invalidateQueries({ queryKey: ["export-runs"] });
    } catch (e) {
      if (e instanceof UnauthorizedError) {
        // Same refresh dance as the history fetch.
        window.location.assign(`/api/auth/refresh?next=${encodeURIComponent(pathname)}`);
        return;
      }
      setTriggerState("failed");
    }
  }

  const rows = data?.runs ?? [];

  return (
    <div
      className="overflow-x-auto rounded-xl border border-slate-200 bg-white"
      data-testid="export-runs-table"
    >
      {canTrigger && (
        <div className="flex items-center justify-end gap-3 border-b border-slate-200 px-4 py-3">
          {triggerState === "failed" && (
            <span className="text-xs text-red-600" data-testid="trigger-error">
              {copy.triggerFailed}
            </span>
          )}
          <button
            type="button"
            disabled={triggerState === "busy"}
            onClick={() => void trigger()}
            className="rounded-lg bg-slate-900 px-3 py-1.5 text-xs font-medium text-white hover:bg-slate-700 disabled:opacity-50"
            data-testid="trigger-export"
          >
            {triggerState === "busy" ? copy.triggering : copy.trigger}
          </button>
        </div>
      )}
      {isPending ? (
        <div className="px-6 py-10 text-center text-sm text-slate-500" data-testid="state-loading" role="status">
          {copy.loading}
        </div>
      ) : isError ? (
        <div className="px-6 py-10 text-center text-sm text-red-600" role="alert" data-testid="state-error">
          {unauthorized ? (
            <p>{copy.sessionExpired}</p>
          ) : (
            <div className="flex flex-col items-center gap-2">
              <p>{copy.error}</p>
              <button
                type="button"
                onClick={() => void refetch()}
                className="rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-50"
                data-testid="retry"
              >
                {copy.retry}
              </button>
            </div>
          )}
        </div>
      ) : rows.length === 0 ? (
        <div className="px-6 py-10 text-center text-sm text-slate-500" data-testid="state-empty">
          {copy.empty}
        </div>
      ) : (
        <table className="w-full text-left text-sm">
          <thead className="border-b border-slate-200 text-xs uppercase tracking-wide text-slate-400">
            <tr>
              <th className="px-4 py-3 font-medium">Scheduled</th>
              <th className="px-4 py-3 font-medium">Status</th>
              <th className="px-4 py-3 font-medium">Rows</th>
              <th className="px-4 py-3 font-medium">Notes</th>
              <th className="px-4 py-3 font-medium text-right">File</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-slate-100">
            {rows.map((run) => (
              <ExportRunRow key={run.run_id} run={run} copy={copy} />
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

function shortDateTime(iso: string): string {
  if (!iso) return "—";
  return new Date(iso).toLocaleString("id-ID", { dateStyle: "medium", timeStyle: "short" });
}

// ExportRunRow renders one run: badges for status and the missed-slot
// flag, the error message of a failed run (#53's error display reads it
// from here), and the download button for successful runs.
function ExportRunRow({
  run,
  copy,
}: {
  run: ExportRunsPage["runs"][number];
  copy: ExportRunsTableCopy;
}) {
  const [state, setState] = useState<"idle" | "busy" | "failed">("idle");

  async function download() {
    setState("busy");
    try {
      const res = await fetch(`/api/exports/runs/${encodeURIComponent(run.run_id)}/download-url`);
      if (res.status === 401) {
        window.location.assign("/api/auth/refresh");
        return;
      }
      if (!res.ok) {
        setState("failed");
        return;
      }
      const body = (await res.json()) as { download?: { url?: string } };
      const url = body.download?.url;
      if (!url) {
        setState("failed");
        return;
      }
      window.location.assign(url); // presigned GET on MinIO — bytes bypass the BFF
    } catch {
      setState("failed");
    }
  }

  return (
    <tr data-testid={`run-${run.run_id}`}>
      <td className="px-4 py-3 whitespace-nowrap text-slate-700">{shortDateTime(run.scheduled_for)}</td>
      <td className="px-4 py-3 whitespace-nowrap">
        <span
          className={
            "inline-flex rounded-full px-2 py-0.5 text-xs font-medium " +
            (run.status === "success"
              ? "bg-emerald-50 text-emerald-700"
              : "bg-red-50 text-red-700")
          }
          data-testid="run-status"
        >
          {run.status}
        </span>
        {run.missed_previous && (
          <span
            className="ml-2 inline-flex rounded-full bg-amber-50 px-2 py-0.5 text-xs font-medium text-amber-700"
            title="The previous scheduled slot had no successful run (missed-run detection, PRD §14.8)"
            data-testid="run-missed"
          >
            missed
          </span>
        )}
        {run.trigger === "manual" && (
          <span
            className="ml-2 inline-flex rounded-full bg-slate-100 px-2 py-0.5 text-xs font-medium text-slate-600"
            title={copy.manualBadge}
            data-testid="run-manual"
          >
            {copy.manualBadge}
          </span>
        )}
      </td>
      <td className="px-4 py-3 text-slate-700">{run.status === "success" ? run.row_count : "—"}</td>
      <td className="max-w-md px-4 py-3 text-slate-500">
        {run.status === "failed" ? (
          <span className="text-red-600" data-testid="run-error">
            {run.error_message || "export failed"}
          </span>
        ) : (
          "—"
        )}
      </td>
      <td className="px-4 py-3 text-right">
        {run.status === "success" ? (
          <div className="flex flex-col items-end gap-1">
            <button
              type="button"
              disabled={state === "busy"}
              onClick={() => void download()}
              className="rounded-lg bg-slate-900 px-3 py-1.5 text-xs font-medium text-white hover:bg-slate-700 disabled:opacity-50"
              data-testid="run-download"
            >
              {state === "busy" ? copy.downloading : copy.download}
            </button>
            {state === "failed" && (
              <span className="text-xs text-red-600" data-testid="run-download-error">
                {copy.downloadFailed}
              </span>
            )}
          </div>
        ) : (
          <span className="text-xs text-slate-400">—</span>
        )}
      </td>
    </tr>
  );
}
