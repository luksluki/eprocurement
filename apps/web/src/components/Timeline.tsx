"use client";

// Timeline (issue #43, FR-11): the shared event-history timeline for any
// aggregate (PR today; PO/invoice later). Generic over entityType/entityId —
// the host page (e.g. the requisition detail, issue #34) owns the surrounding
// layout and passes localized copy as props (client components never import
// the i18n dictionaries). Pages arrive newest-first from /api/events; "Load
// more" walks the keyset next_cursor; polling every 2 s keeps the timeline
// within FR-11's "state changes appear within 2 s" budget while the outbox
// pipeline (#45–#47) lands.
import { useInfiniteQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";
import { usePathname } from "next/navigation";
import type { EventsPage } from "@/lib/gateway";
import { formatRelativeTime } from "@/lib/relative-time";

// FR-11: every state change appears within 2 s. The interval must stay <= 2000.
export const TIMELINE_POLL_INTERVAL_MS = 2000;

export type TimelineCopy = {
  title: string;
  empty: string;
  loading: string;
  sessionExpired: string;
  error: string;
  retry: string;
  loadMore: string;
  loadingMore: string;
  filterLabel: string;
  filterAll: string;
};

export type TimelineProps = {
  entityType: string;
  entityId: string;
  locale: string; // BCP 47 tag for relative timestamps ("id" | "en")
  copy: TimelineCopy;
  limit?: number; // page size, defaults to the gateway's 20
};

class UnauthorizedError extends Error {
  constructor() {
    super("unauthorized");
  }
}

async function fetchEventsPage(
  entityType: string,
  entityId: string,
  limit: number | undefined,
  cursor: string,
): Promise<EventsPage> {
  const params = new URLSearchParams({ entityType, entityId });
  if (limit) {
    params.set("limit", String(limit));
  }
  if (cursor) {
    params.set("cursor", cursor);
  }
  const res = await fetch(`/api/events?${params.toString()}`);
  if (res.status === 401) {
    throw new UnauthorizedError(); // triggers the refresh dance below
  }
  if (!res.ok) {
    throw new Error(`events failed (${res.status})`);
  }
  return (await res.json()) as EventsPage;
}

export default function Timeline({ entityType, entityId, locale, copy, limit }: TimelineProps) {
  const pathname = usePathname();

  const { data, isPending, isError, error, isFetchingNextPage, hasNextPage, refetch, fetchNextPage } =
    useInfiniteQuery({
      queryKey: ["events", entityType, entityId],
      queryFn: ({ pageParam }) => fetchEventsPage(entityType, entityId, limit, pageParam),
      initialPageParam: "",
      getNextPageParam: (last) => last.next_cursor || undefined,
      refetchInterval: TIMELINE_POLL_INTERVAL_MS,
      retry: false,
    });

  // A 401 from the BFF means the access token expired: run the refresh dance
  // (browser navigation that rotates the cookies and returns to this page).
  const unauthorized = error instanceof UnauthorizedError;
  useEffect(() => {
    if (unauthorized) {
      window.location.assign(`/api/auth/refresh?next=${encodeURIComponent(pathname)}`);
    }
  }, [unauthorized, pathname]);

  const events = useMemo(() => data?.pages.flatMap((page) => page.events) ?? [], [data]);
  const types = useMemo(() => [...new Set(events.map((event) => event.type))], [events]);
  const [typeFilter, setTypeFilter] = useState("all");
  const visible = useMemo(
    () => (typeFilter === "all" ? events : events.filter((event) => event.type === typeFilter)),
    [events, typeFilter],
  );

  return (
    <section aria-label={copy.title} data-testid="timeline">
      {types.length > 1 ? (
        <label className="flex items-center gap-2 text-xs font-medium text-slate-500">
          {copy.filterLabel}
          <select
            value={typeFilter}
            onChange={(e) => setTypeFilter(e.target.value)}
            className="rounded-lg border border-slate-200 bg-white px-2 py-1 text-sm text-slate-900"
            data-testid="timeline-filter"
          >
            <option value="all">{copy.filterAll}</option>
            {types.map((type) => (
              <option key={type} value={type}>
                {type}
              </option>
            ))}
          </select>
        </label>
      ) : null}

      {isPending ? (
        <div className="mt-3 space-y-2" data-testid="timeline-loading" role="status">
          {[0, 1, 2].map((i) => (
            <div key={i} className="h-10 animate-pulse rounded-lg bg-slate-100" />
          ))}
          <span className="sr-only">{copy.loading}</span>
        </div>
      ) : isError ? (
        <div className="mt-3 px-4 py-8 text-center text-sm text-red-600" role="alert" data-testid="timeline-error">
          {unauthorized ? (
            <p>{copy.sessionExpired}</p>
          ) : (
            <div className="flex flex-col items-center gap-2">
              <p>{copy.error}</p>
              <button
                type="button"
                onClick={() => void refetch()}
                className="rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-50"
                data-testid="timeline-retry"
              >
                {copy.retry}
              </button>
            </div>
          )}
        </div>
      ) : events.length === 0 ? (
        <div className="mt-3 px-4 py-8 text-center text-sm text-slate-500" data-testid="timeline-empty">
          {copy.empty}
        </div>
      ) : (
        <>
          <ol className="mt-3 space-y-0" data-testid="timeline-items">
            {visible.map((event) => (
              <li
                key={event.event_id}
                className="relative border-l border-slate-200 pb-4 pl-4 last:pb-0"
                data-testid="timeline-item"
              >
                <span
                  className="absolute -left-[5px] top-1.5 h-2.5 w-2.5 rounded-full bg-slate-400"
                  aria-hidden="true"
                />
                <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
                  <span className="rounded-full bg-slate-100 px-2 py-0.5 text-xs font-medium text-slate-700">
                    {event.type}
                  </span>
                  <time
                    className="text-xs text-slate-500"
                    dateTime={event.occurred_at}
                    data-testid="timeline-time"
                  >
                    {formatRelativeTime(event.occurred_at, locale)}
                  </time>
                </div>
                <p className="mt-1 text-xs text-slate-400">
                  {event.actor_id ? `${event.actor_id} · ` : ""}
                  {event.occurred_at}
                </p>
              </li>
            ))}
          </ol>
          {visible.length === 0 && typeFilter !== "all" ? (
            <div className="mt-3 px-4 py-6 text-center text-sm text-slate-500" data-testid="timeline-filter-empty">
              {copy.empty}
            </div>
          ) : null}
          {hasNextPage ? (
            <button
              type="button"
              disabled={isFetchingNextPage}
              onClick={() => void fetchNextPage()}
              className="mt-3 rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-50 disabled:cursor-not-allowed disabled:opacity-40"
              data-testid="timeline-load-more"
            >
              {isFetchingNextPage ? copy.loadingMore : copy.loadMore}
            </button>
          ) : null}
        </>
      )}
    </section>
  );
}
