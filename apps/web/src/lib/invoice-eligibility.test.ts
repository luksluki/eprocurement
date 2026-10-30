// invoice-eligibility tests (issue #41): the hidden/shown tables for the
// match action, the create link, and the hard attachment gate — the
// po-eligibility (#38) conventions.
import { describe, expect, it } from "vitest";
import type { AttachmentMeta } from "@/lib/gateway";
import { canCreateInvoice, canMatchInvoice, hasReadyAttachment, holdsInvoiceWrite } from "./invoice-eligibility";

const attachment = (status: string): AttachmentMeta => ({
  id: "att-1",
  entity_type: "invoice",
  entity_id: "inv-1",
  filename: "scan.pdf",
  mime_type: "application/pdf",
  size_bytes: 12,
  checksum_sha256: "ab",
  status,
  created_by: "u-1",
  created_at: "2026-10-29T10:00:00Z",
  uploaded_at: "2026-10-29T10:01:00Z",
});

describe("holdsInvoiceWrite", () => {
  it("grants finance and admin only", () => {
    expect(holdsInvoiceWrite(["finance"])).toBe(true);
    expect(holdsInvoiceWrite(["admin"])).toBe(true);
    expect(holdsInvoiceWrite(["finance", "viewer"])).toBe(true);
    expect(holdsInvoiceWrite(["requester"])).toBe(false);
    expect(holdsInvoiceWrite(["approver"])).toBe(false);
    expect(holdsInvoiceWrite(["buyer"])).toBe(false);
    expect(holdsInvoiceWrite(["viewer"])).toBe(false);
    expect(holdsInvoiceWrite([])).toBe(false);
  });
});

describe("canMatchInvoice", () => {
  const table: { roles: string[]; status: string; want: boolean }[] = [
    { roles: ["finance"], status: "pending", want: true },
    { roles: ["admin"], status: "pending", want: true },
    { roles: ["finance"], status: "discrepancy", want: true },
    // matched is final: the re-run guard answers 409 server-side.
    { roles: ["finance"], status: "matched", want: false },
    // invoice:write holders only.
    { roles: ["buyer"], status: "pending", want: false },
    { roles: ["viewer"], status: "pending", want: false },
    { roles: ["requester"], status: "pending", want: false },
    { roles: ["approver"], status: "pending", want: false },
  ];
  it.each(table)("($roles, $status) -> $want", ({ roles, status, want }) => {
    expect(canMatchInvoice(roles, status)).toBe(want);
  });
});

describe("canCreateInvoice", () => {
  const table: { roles: string[]; poStatus: string; want: boolean }[] = [
    { roles: ["finance"], poStatus: "acknowledged", want: true },
    { roles: ["finance"], poStatus: "partially_received", want: true },
    { roles: ["finance"], poStatus: "received", want: true },
    { roles: ["admin"], poStatus: "received", want: true },
    // Non-invoicable PO statuses (the #39 GRN set gate).
    { roles: ["finance"], poStatus: "draft", want: false },
    { roles: ["finance"], poStatus: "issued", want: false },
    { roles: ["finance"], poStatus: "closed", want: false },
    { roles: ["finance"], poStatus: "cancelled", want: false },
    // invoice:write holders only.
    { roles: ["buyer"], poStatus: "received", want: false },
    { roles: ["viewer"], poStatus: "received", want: false },
  ];
  it.each(table)("($roles, $poStatus) -> $want", ({ roles, poStatus, want }) => {
    expect(canCreateInvoice(roles, poStatus)).toBe(want);
  });
});

describe("hasReadyAttachment (the hard gate's condition)", () => {
  it("is false without attachments and for pending-only uploads", () => {
    expect(hasReadyAttachment(undefined)).toBe(false);
    expect(hasReadyAttachment([])).toBe(false);
    expect(hasReadyAttachment([attachment("pending")])).toBe(false);
  });

  it("is true once any attachment reached ready", () => {
    expect(hasReadyAttachment([attachment("ready")])).toBe(true);
    expect(hasReadyAttachment([attachment("pending"), attachment("ready")])).toBe(true);
  });
});
