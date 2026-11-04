// InvoiceAttachments tests (issue #41): the list (filename/size/status), the
// download flow (BFF → presigned URL), and the finance upload — the #33
// declare → presigned PUT → complete flow, whose completion refreshes the
// panel (the ready state unlocks the Match button's hard gate).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { AttachmentMeta } from "@/lib/gateway";
import InvoiceAttachments from "./InvoiceAttachments";

const mocks = vi.hoisted(() => ({
  refresh: vi.fn(),
  assign: vi.fn(),
}));

vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: mocks.refresh }),
}));

const copy = {
  heading: "Attachments",
  empty: "No attachments yet.",
  columns: { filename: "Filename", size: "Size", status: "Status", download: "Download" },
  downloadFailed: "Could not fetch the download link — try again.",
  uploadLabel: "Upload the vendor invoice document",
  uploading: "Uploading…",
};

const pending: AttachmentMeta = {
  id: "att-1",
  entity_type: "invoice",
  entity_id: "inv-1",
  filename: "scan.pdf",
  mime_type: "application/pdf",
  size_bytes: 2048,
  checksum_sha256: "ab",
  status: "pending",
  created_by: "u-1",
  created_at: "2026-10-29T10:00:00Z",
  uploaded_at: "2026-10-29T10:01:00Z",
};

const ready: AttachmentMeta = { ...pending, id: "att-2", filename: "scan2.pdf", status: "ready" };

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

const originalLocation = window.location;

beforeEach(() => {
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { ...originalLocation, assign: mocks.assign },
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  mocks.refresh.mockClear();
  mocks.assign.mockClear();
  Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
});

describe("InvoiceAttachments", () => {
  it("renders the empty state when there are no attachments", () => {
    render(<InvoiceAttachments invoiceId="inv-1" attachments={[]} canUpload={false} copy={copy} />);
    expect(screen.getByTestId("invoice-attachments-empty").textContent).toBe(copy.empty);
  });

  it("lists filename, size, and status per attachment", () => {
    render(<InvoiceAttachments invoiceId="inv-1" attachments={[pending, ready]} canUpload={false} copy={copy} />);
    const table = screen.getByTestId("invoice-attachments-table");
    expect(table.textContent).toContain("scan.pdf");
    expect(table.textContent).toContain("2.0 KB");
    expect(table.textContent).toContain("pending");
    expect(table.textContent).toContain("ready");
  });

  it("mints the presigned URL through the BFF and navigates to it on download", async () => {
    const fetchMock = vi.fn(async () =>
      jsonResponse({ id: "att-2", status: "ready", download: { url: "http://minio:9000/att?sig=1" } }),
    );
    vi.stubGlobal("fetch", fetchMock);

    render(<InvoiceAttachments invoiceId="inv-1" attachments={[ready]} canUpload={false} copy={copy} />);
    fireEvent.click(screen.getByTestId("invoice-attachment-download-att-2"));

    await waitFor(() => expect(mocks.assign).toHaveBeenCalledWith("http://minio:9000/att?sig=1"));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0][0]).toBe("/api/invoices/inv-1/attachments/att-2/download");
    expect(screen.queryByTestId("invoice-download-error")).toBeNull();
  });

  it("shows the download failure inline when the BFF answers non-OK", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "not ready", code: "attachment_not_ready" }, 412)));

    render(<InvoiceAttachments invoiceId="inv-1" attachments={[pending]} canUpload={false} copy={copy} />);
    fireEvent.click(screen.getByTestId("invoice-attachment-download-att-1"));

    const error = await screen.findByTestId("invoice-download-error");
    expect(error.textContent).toBe(copy.downloadFailed);
    expect(mocks.assign).not.toHaveBeenCalled();
  });

  it("hides the upload control from viewers (canUpload false)", () => {
    render(<InvoiceAttachments invoiceId="inv-1" attachments={[]} canUpload={false} copy={copy} />);
    expect(screen.queryByTestId("invoice-upload")).toBeNull();
  });

  it("runs the declare → presigned PUT → complete flow for finance and refreshes", async () => {
    const file = new File(["invoice-bytes"], "scan.pdf", { type: "application/pdf" });
    const fetchMock = vi
      .fn()
      // 1. declare through the BFF → the presigned envelope
      .mockImplementationOnce(async () =>
        jsonResponse(
          {
            id: "att-3",
            status: "pending",
            upload: { url: "http://minio:9000/att-3", method: "PUT", expires_at: "soon", headers: {} },
          },
          201,
        ),
      )
      // 2. PUT straight to MinIO
      .mockImplementationOnce(async () => new Response(null, { status: 200 }))
      // 3. complete through the BFF → ready
      .mockImplementationOnce(async () => jsonResponse({ id: "att-3", status: "ready" }));
    vi.stubGlobal("fetch", fetchMock);

    render(<InvoiceAttachments invoiceId="inv-1" attachments={[]} canUpload={true} copy={copy} />);
    fireEvent.change(screen.getByTestId("invoice-upload-input"), { target: { files: [file] } });

    await waitFor(() => expect(mocks.refresh).toHaveBeenCalledTimes(1));
    const urls = fetchMock.mock.calls.map((c) => c[0] as string);
    expect(urls[0]).toBe("/api/invoices/inv-1/attachments");
    expect(urls[1]).toBe("http://minio:9000/att-3");
    expect(urls[2]).toBe("/api/invoices/inv-1/attachments/att-3/complete");
    // The declaration carries the file facts (checksum computed client-side).
    const declareBody = JSON.parse((fetchMock.mock.calls[0][1] as RequestInit).body as string) as {
      filename: string;
      size_bytes: number;
      checksum_sha256: string;
    };
    expect(declareBody.filename).toBe("scan.pdf");
    expect(declareBody.size_bytes).toBe(file.size);
    expect(declareBody.checksum_sha256).toMatch(/^[0-9a-f]{64}$/);
    expect(screen.queryByTestId("invoice-upload-error")).toBeNull();
  });

  it("maps a declare failure to its human sentence without refreshing", async () => {
    const file = new File(["x"], "scan.exe", { type: "application/octet-stream" });
    render(<InvoiceAttachments invoiceId="inv-1" attachments={[]} canUpload={true} copy={copy} />);
    fireEvent.change(screen.getByTestId("invoice-upload-input"), { target: { files: [file] } });

    // The mime allowlist rejects before any network call (#33 flow).
    const error = await screen.findByTestId("invoice-upload-error");
    expect(error.textContent).toContain("not allowed");
    expect(mocks.refresh).not.toHaveBeenCalled();
  });
});
