import { describe, expect, it, vi } from "vitest";
import { ATTACHMENT_MIME_TYPES, uploadAttachment, uploadErrorMessage, type UploadApi } from "./upload";
import type { AttachmentMeta, DeclaredUpload } from "@/lib/gateway";

function makeFile(type: string): File {
  return new File(["0123456789"], "quote.pdf", { type });
}

const declared: DeclaredUpload = {
  id: "att-1",
  entity_type: "requisition",
  entity_id: "pr-1",
  filename: "quote.pdf",
  mime_type: "application/pdf",
  size_bytes: 10,
  checksum_sha256: "ab".repeat(32),
  status: "pending",
  created_by: "u-1",
  created_at: "2026-10-25T10:00:00Z",
  uploaded_at: "",
  upload: {
    url: "http://minio:9000/attachments/t/requisition/att-1?X-Amz-Signature=sig",
    method: "PUT",
    expires_at: "2026-10-25T10:15:00Z",
    headers: { "Content-Type": "application/pdf" },
  },
};

const ready: AttachmentMeta = { ...declared, status: "ready", uploaded_at: "2026-10-25T10:01:00Z" };

function api(overrides: Partial<UploadApi> = {}): UploadApi {
  return {
    declare: vi.fn(async () => ({ ok: true as const, data: declared })),
    put: vi.fn(async () => true),
    complete: vi.fn(async () => ({ ok: true as const, data: ready })),
    ...overrides,
  };
}

const digest = async () => "ab".repeat(32);

describe("uploadAttachment (declare → PUT → complete, #33)", () => {
  it("runs the happy path: declare with checksum, PUT with the signed content type, complete", async () => {
    const mock = api();
    const outcome = await uploadAttachment(makeFile("application/pdf"), mock, digest);
    expect(outcome.ok).toBe(true);
    if (!outcome.ok) return;
    expect(outcome.attachment.status).toBe("ready");
    expect(mock.declare).toHaveBeenCalledWith({
      filename: "quote.pdf",
      mime_type: "application/pdf",
      size_bytes: 10,
      checksum_sha256: "ab".repeat(32),
    });
    expect(mock.put).toHaveBeenCalledWith(
      declared.upload.url,
      "application/pdf", // from the response's headers map, not the file guess
      expect.any(File),
    );
    expect(mock.complete).toHaveBeenCalledWith("att-1");
  });

  it("fails fast on a mime type outside the allowlist (no network calls)", async () => {
    const mock = api();
    const outcome = await uploadAttachment(makeFile("application/x-msdownload"), mock, digest);
    expect(outcome.ok).toBe(false);
    if (outcome.ok) return;
    expect(outcome.code).toBe("upload_mime_unsupported");
    expect(mock.declare).not.toHaveBeenCalled();
  });

  it("maps a declare rejection (attachment_too_large) to a human message", async () => {
    const mock = api({
      declare: vi.fn(async () => ({
        ok: false as const,
        status: 400,
        error: "file exceeds the size limit",
        code: "attachment_too_large",
      })),
    });
    const outcome = await uploadAttachment(makeFile("application/pdf"), mock, digest);
    expect(outcome.ok).toBe(false);
    if (outcome.ok) return;
    expect(outcome.code).toBe("attachment_too_large");
    expect(outcome.message).toBe(uploadErrorMessage("attachment_too_large"));
    expect(mock.put).not.toHaveBeenCalled();
  });

  it("maps a failed presigned PUT to upload_put_failed", async () => {
    const mock = api({ put: vi.fn(async () => false) });
    const outcome = await uploadAttachment(makeFile("application/pdf"), mock, digest);
    expect(outcome).toMatchObject({ ok: false, code: "upload_put_failed" });
  });

  it("maps complete-time verification failures (checksum mismatch)", async () => {
    const mock = api({
      complete: vi.fn(async () => ({
        ok: false as const,
        status: 412,
        error: "uploaded content does not match the declared checksum",
        code: "attachment_checksum_mismatch",
      })),
    });
    const outcome = await uploadAttachment(makeFile("application/pdf"), mock, digest);
    expect(outcome).toMatchObject({ ok: false, code: "attachment_checksum_mismatch" });
    if (!outcome.ok) return;
    expect(outcome.message).toContain("checksum");
  });

  it("attachment_entity_invalid renders the neutralized message (shared vendor map)", () => {
    expect(uploadErrorMessage("attachment_entity_invalid")).toBe("Attachment owner type is not supported.");
    expect(uploadErrorMessage("unknown_code_xyz")).toContain("unknown_code_xyz");
    expect(ATTACHMENT_MIME_TYPES).toContain("application/pdf");
  });
});
