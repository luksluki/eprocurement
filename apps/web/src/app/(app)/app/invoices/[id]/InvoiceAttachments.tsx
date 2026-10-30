"use client";

// Attachments panel for the invoice detail page (issue #41): the embedded
// list (filename / size / status / download) plus the finance upload — the
// #33 flow verbatim (declare through the BFF → PUT straight to the MinIO
// presigned URL → complete verifies server-side), so the vendor's invoice
// document can reach ready state and unlock the Match button (the hard gate
// lives in InvoiceActions). A successful upload refreshes the server-rendered
// panel.
import { useRef, useState } from "react";
import { useRouter } from "next/navigation";
import type { AttachmentMeta } from "@/lib/gateway";
import { parseApiResult, uploadAttachment } from "@/app/(app)/app/requisitions/new/upload";

export type InvoiceAttachmentsCopy = {
  heading: string;
  empty: string;
  columns: { filename: string; size: string; status: string; download: string };
  downloadFailed: string;
  uploadLabel: string;
  uploading: string;
};

// bytes renders a human-readable size (the requisition detail's helper).
function bytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

export default function InvoiceAttachments({
  invoiceId,
  attachments,
  canUpload,
  copy,
}: {
  invoiceId: string;
  attachments: AttachmentMeta[];
  canUpload: boolean;
  copy: InvoiceAttachmentsCopy;
}) {
  const router = useRouter();
  const fileInput = useRef<HTMLInputElement>(null);
  const [uploading, setUploading] = useState(false);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const [downloadError, setDownloadError] = useState<string | null>(null);

  // The #33 upload flow against the invoice's BFF routes: declare → PUT to
  // the presigned URL → complete. uploadAttachment maps every hop's machine
  // code to a human sentence.
  async function upload(file: File) {
    setUploading(true);
    setUploadError(null);
    const outcome = await uploadAttachment(file, {
      async declare(declaration) {
        const res = await fetch(`/api/invoices/${encodeURIComponent(invoiceId)}/attachments`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify(declaration),
        });
        return parseApiResult(res);
      },
      async put(url, contentType, f) {
        const res = await fetch(url, { method: "PUT", headers: { "content-type": contentType }, body: f });
        return res.ok;
      },
      async complete(attachmentId) {
        const res = await fetch(
          `/api/invoices/${encodeURIComponent(invoiceId)}/attachments/${encodeURIComponent(attachmentId)}/complete`,
          { method: "POST" },
        );
        return parseApiResult(res);
      },
    });
    setUploading(false);
    if (!outcome.ok) {
      setUploadError(outcome.message);
      return;
    }
    if (fileInput.current) fileInput.current.value = "";
    router.refresh(); // the panel and the Match gate re-render with `ready`
  }

  // Download mints the short-lived presigned GET through the BFF (the
  // browser never holds the access token while talking to MinIO), then
  // navigates to it.
  async function download(attachment: AttachmentMeta) {
    setDownloadError(null);
    try {
      const res = await fetch(
        `/api/invoices/${encodeURIComponent(invoiceId)}/attachments/${encodeURIComponent(attachment.id)}/download`,
        { cache: "no-store" },
      );
      if (!res.ok) {
        setDownloadError(copy.downloadFailed);
        return;
      }
      const body = (await res.json()) as { download?: { url?: string } };
      if (!body.download?.url) {
        setDownloadError(copy.downloadFailed);
        return;
      }
      window.location.assign(body.download.url);
    } catch {
      setDownloadError(copy.downloadFailed);
    }
  }

  const statusTone = (status: string) =>
    status === "ready"
      ? "bg-emerald-50 text-emerald-700"
      : status === "pending"
        ? "bg-amber-50 text-amber-700"
        : "bg-slate-100 text-slate-500";

  return (
    <section className="mt-8" data-testid="invoice-attachments">
      <h2 className="text-lg font-semibold">{copy.heading}</h2>
      {attachments.length === 0 ? (
        <p className="mt-2 text-sm text-slate-500" data-testid="invoice-attachments-empty">
          {copy.empty}
        </p>
      ) : (
        <table className="mt-2 w-full text-left text-sm" data-testid="invoice-attachments-table">
          <thead className="border-b border-slate-200 text-xs uppercase tracking-wide text-slate-400">
            <tr>
              <th className="py-2 pr-4 font-medium">{copy.columns.filename}</th>
              <th className="py-2 pr-4 text-right font-medium">{copy.columns.size}</th>
              <th className="py-2 pr-4 font-medium">{copy.columns.status}</th>
              <th className="py-2 font-medium">{copy.columns.download}</th>
            </tr>
          </thead>
          <tbody>
            {attachments.map((attachment) => (
              <tr key={attachment.id} className="border-b border-slate-100 last:border-0">
                <td className="py-2 pr-4 font-medium">{attachment.filename}</td>
                <td className="py-2 pr-4 text-right text-slate-500">{bytes(attachment.size_bytes)}</td>
                <td className="py-2 pr-4">
                  <span className={`rounded-full px-2 py-0.5 text-xs font-medium ${statusTone(attachment.status)}`}>
                    {attachment.status}
                  </span>
                </td>
                <td className="py-2">
                  <button
                    type="button"
                    onClick={() => void download(attachment)}
                    className="text-sm font-medium text-slate-700 underline"
                    data-testid={`invoice-attachment-download-${attachment.id}`}
                  >
                    {copy.columns.download}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {downloadError ? (
        <p className="mt-2 text-sm text-red-600" role="alert" data-testid="invoice-download-error">
          {downloadError}
        </p>
      ) : null}

      {canUpload ? (
        <div className="mt-3 flex flex-wrap items-center gap-3" data-testid="invoice-upload">
          <label className="flex flex-col gap-1 text-xs font-medium text-slate-500">
            {copy.uploadLabel}
            <input
              ref={fileInput}
              type="file"
              onChange={(e) => {
                const file = e.target.files?.[0];
                if (file) void upload(file);
              }}
              disabled={uploading}
              className="text-sm"
              data-testid="invoice-upload-input"
            />
          </label>
          {uploading ? (
            <span className="text-xs text-slate-500" data-testid="invoice-upload-pending">
              {copy.uploading}
            </span>
          ) : null}
        </div>
      ) : null}
      {uploadError ? (
        <p className="mt-2 text-sm text-red-600" role="alert" data-testid="invoice-upload-error">
          {uploadError}
        </p>
      ) : null}
    </section>
  );
}
