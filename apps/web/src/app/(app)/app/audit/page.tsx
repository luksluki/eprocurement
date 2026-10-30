import Link from "next/link";
import {
  Badge,
  DataTable,
  EmptyState,
  ErrorState,
  PageHeader,
  type DataTableColumn,
} from "@eprocurement/ui";
import { getDictionary, getLocale } from "@/i18n/server";
import { listAudit, type AuditEntryWire } from "@/lib/gateway";
import { requireSession } from "@/lib/session";

export const dynamic = "force-dynamic";

// The five audit actions of PRD §14.7 (services/documents/history/audit.go
// AuditAction) — the filter dropdown's option set. Badge tones: approve
// success, reject danger, the operational verbs neutral, match success.
const AUDIT_ACTIONS: ReadonlyArray<{ value: string; tone: "success" | "danger" | "neutral" }> = [
  { value: "approve", tone: "success" },
  { value: "reject", tone: "danger" },
  { value: "issue", tone: "neutral" },
  { value: "post", tone: "neutral" },
  { value: "match", tone: "success" },
];

// PRD §14.7 / issue #44: /app/audit — the decision audit log for
// audit:read holders (viewer + admin; the nav entry renders only for them,
// and the gateway plus documents re-check the permission on every call).
// A server component rendering one keyset page of GET /api/v1/audit through
// the design-system DataTable; the action filter lives in the URL through a
// plain GET <form> (no client JS). Pagination is keyset like the timeline
// (dev-note 042): "Older" continues from next_cursor, "Newest" resets —
// an append-only stream has no meaningful page numbers.
export default async function AuditPage({
  searchParams,
}: {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const params = await searchParams;
  const first = (key: string) => {
    const v = params[key];
    return (Array.isArray(v) ? v[0] : v) ?? "";
  };
  const action = AUDIT_ACTIONS.some((a) => a.value === first("action")) ? first("action") : "";
  const cursor = first("cursor").trim();

  const { accessToken } = await requireSession("/app/audit");
  const [locale, dict] = await Promise.all([getLocale(), getDictionary()]);
  const t = dict.auditPage;

  const query = new URLSearchParams();
  if (action) query.set("action", action);
  if (cursor) query.set("cursor", cursor);
  const result = await listAudit(accessToken, query.toString());

  const localeTag = locale === "id" ? "id-ID" : "en-US";
  const dateTime = (iso: string) =>
    iso ? new Intl.DateTimeFormat(localeTag, { dateStyle: "medium", timeStyle: "short" }).format(new Date(iso)) : "—";

  const badgeTone = (value: string): "success" | "danger" | "neutral" =>
    AUDIT_ACTIONS.find((a) => a.value === value)?.tone ?? "neutral";

  const columns: DataTableColumn<AuditEntryWire>[] = [
    {
      key: "occurred_at",
      label: t.columns.occurredAt,
      render: (e) => <span className="whitespace-nowrap text-ink-subtle">{dateTime(e.occurred_at)}</span>,
    },
    {
      key: "actor_id",
      label: t.columns.actor,
      render: (e) => <span className="font-mono text-xs">{e.actor_id || "—"}</span>,
    },
    {
      key: "action",
      label: t.columns.action,
      render: (e) => <Badge tone={badgeTone(e.action)}>{e.action}</Badge>,
    },
    {
      key: "entity",
      label: t.columns.entity,
      render: (e) => (
        <span className="font-mono text-xs">
          {e.entity_type}/{e.entity_id.slice(0, 8)}
        </span>
      ),
    },
    {
      key: "actor_ip",
      label: t.columns.ip,
      render: (e) => <span className="font-mono text-xs">{e.actor_ip || "—"}</span>,
    },
    {
      key: "trace_id",
      label: t.columns.trace,
      render: (e) => (
        <span className="font-mono text-xs text-ink-subtle">{e.trace_id ? e.trace_id.slice(0, 16) : "—"}</span>
      ),
    },
  ];

  // Keyset links keep the active filter; "Older" continues from the page's
  // cursor, "Newest" drops it.
  const hrefFor = (next: string) => {
    const q = new URLSearchParams();
    if (action) q.set("action", action);
    if (next) q.set("cursor", next);
    const qs = q.toString();
    return qs ? `/app/audit?${qs}` : "/app/audit";
  };

  const entries = result.ok ? result.data.entries : [];
  const nextCursor = result.ok ? result.data.next_cursor : "";

  return (
    <section aria-labelledby="audit-title">
      <PageHeader id="audit-title" title={dict.pages.audit.title} description={dict.pages.audit.description} />

      <div className="mt-4 flex flex-wrap items-center justify-between gap-3">
        {/* A plain GET form: the action filter navigates by URL, no client JS. */}
        <form method="get" action="/app/audit" className="flex flex-wrap items-end gap-3">
          <label className="flex flex-col gap-1 text-xs font-medium text-slate-500">
            {t.filters.actionLabel}
            <select
              name="action"
              defaultValue={action}
              className="rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-900"
              data-testid="filter-action"
            >
              <option value="">{t.filters.actionAll}</option>
              {AUDIT_ACTIONS.map((a) => (
                <option key={a.value} value={a.value}>
                  {a.value}
                </option>
              ))}
            </select>
          </label>
          <button
            type="submit"
            className="rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-700"
            data-testid="filter-apply"
          >
            {t.filters.apply}
          </button>
        </form>
        {/* The export is a plain link: the BFF route re-serves documents'
            bounded CSV (metadata columns, newest 10 000 rows) with the
            session cookie riding along automatically. */}
        <a
          href="/api/audit/export"
          className="rounded-lg border border-slate-300 bg-white px-4 py-2 text-sm font-medium text-slate-900 hover:bg-slate-50"
          data-testid="audit-export"
          title={t.exportHint}
        >
          {t.exportCsv}
        </a>
      </div>

      <div className="mt-4 flex flex-col gap-3">
        {!result.ok ? (
          result.status === 403 ? (
            <ErrorState message={t.forbidden} />
          ) : (
            <ErrorState message={t.unavailable} />
          )
        ) : (
          <>
            <DataTable
              data={entries}
              columns={columns}
              getRowId={(e) => e.event_id}
              caption={t.caption}
              emptyState={<EmptyState title={t.empty} body={t.emptyHint} />}
            />
            {nextCursor !== "" ? (
              <div className="flex items-center gap-4 text-sm" data-testid="audit-pagination">
                <Link href={hrefFor(nextCursor)} className="font-medium text-slate-900 underline-offset-2 hover:underline">
                  {t.pagination.next} →
                </Link>
                {cursor !== "" ? (
                  <Link href={hrefFor("")} className="text-slate-500 underline-offset-2 hover:underline">
                    {t.pagination.first}
                  </Link>
                ) : null}
              </div>
            ) : null}
          </>
        )}
      </div>
    </section>
  );
}
