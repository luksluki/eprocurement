import { NextResponse } from "next/server";
import { cookies } from "next/headers";
import { ACCESS_COOKIE } from "@/lib/constants";
import { exportAuditCsv } from "@/lib/gateway";

export const dynamic = "force-dynamic";

// GET /api/audit/export — BFF proxy for the audit CSV download (#44). The
// bytes are buffered by contract (documents caps the export at the newest
// 10 000 rows of bounded columns), so re-serving them with the attachment
// headers is safe; streaming/MinIO delivery is the documented follow-up if
// the cap rises. 401 runs the refresh dance client-side; 403 carries the
// audit:read denial; anything else is the retryable 502.
export async function GET() {
  const store = await cookies();
  const accessToken = store.get(ACCESS_COOKIE)?.value;
  if (!accessToken) {
    return NextResponse.json({ error: "Sign-in required.", code: "unauthorized" }, { status: 401 });
  }

  const result = await exportAuditCsv(accessToken);
  if (!result.ok) {
    if (result.status === 401) {
      return NextResponse.json({ error: "Session expired.", code: "unauthorized" }, { status: 401 });
    }
    if (result.status === 403) {
      return NextResponse.json(
        { error: "The audit export requires the audit:read permission.", code: "permission_denied" },
        { status: 403 },
      );
    }
    return NextResponse.json(
      { error: "Failed to export the audit log from the gateway.", code: "gateway_unavailable" },
      { status: 502 },
    );
  }
  return new NextResponse(result.csv, {
    status: 200,
    headers: {
      "content-type": "text/csv; charset=utf-8",
      "content-disposition": 'attachment; filename="audit-log.csv"',
      "cache-control": "no-store",
    },
  });
}
