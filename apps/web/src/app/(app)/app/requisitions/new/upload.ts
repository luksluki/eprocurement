"use client";

// Attachment upload flow (#33): declare through the BFF (the gateway checks
// the draft exists and is the caller's), PUT the bytes straight to the
// MinIO presigned URL — the file never streams through any API — then
// complete, which verifies size/mime/checksum server-side. Machine codes
// from every hop map to human sentences per step; unknown codes still
// surface with their code instead of crashing.
import type { AttachmentMeta, DeclaredUpload } from "@/lib/gateway";

// Mirrors the documents allowlist (services/documents/attachment):
// pdf, png, jpeg, webp, docx, xlsx.
export const ATTACHMENT_MIME_TYPES = [
  "application/pdf",
  "image/png",
  "image/jpeg",
  "image/webp",
  "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
  "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
] as const;

export const uploadErrorMessages: Record<string, string> = {
  attachment_too_large: "File exceeds the size limit.",
  attachment_mime_invalid: "File type is not allowed (pdf, png, jpeg, webp, docx, xlsx).",
  attachment_filename_invalid: "Filename must be 1-255 characters without path separators.",
  attachment_checksum_invalid: "Could not read the file — try adding it again.",
  attachment_entity_invalid: "Attachment owner type is not supported.",
  attachment_not_found: "Attachment not found.",
  attachment_not_ready: "Attachment is still pending.",
  attachment_object_missing: "Upload did not reach storage — try again.",
  attachment_size_mismatch: "Uploaded size differs from the declared size — re-add the file.",
  attachment_mime_mismatch: "Uploaded content type is not allowed.",
  attachment_checksum_mismatch: "Uploaded content does not match the declared checksum — re-add the file.",
  requisition_not_found: "This draft no longer exists.",
  requisition_forbidden: "Only the requester may attach files to this draft.",
  requisition_illegal_transition: "This requisition is no longer a draft.",
  upload_mime_unsupported: "File type is not allowed (pdf, png, jpeg, webp, docx, xlsx).",
  upload_empty: "The file is empty.",
  upload_put_failed: "Storing the file failed — try again.",
  body_invalid: "The upload request was malformed — try again.",
  gateway_unavailable: "The gateway is unreachable — check your connection and retry.",
};

export function uploadErrorMessage(code: string): string {
  return uploadErrorMessages[code] ?? `Upload failed (${code}).`;
}

export async function sha256Hex(file: File): Promise<string> {
  const buffer = await file.arrayBuffer();
  const digest = await crypto.subtle.digest("SHA-256", buffer);
  return Array.from(new Uint8Array(digest))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}

export type UploadOutcome =
  | { ok: true; attachment: AttachmentMeta }
  | { ok: false; code: string; message: string };

type ApiResult<T> = { ok: true; data: T } | { ok: false; status: number; error: string; code: string };

// UploadApi is the BFF surface; injectable so tests drive the flow without
// fetch mocking gymnastics.
export interface UploadApi {
  declare(declaration: {
    filename: string;
    mime_type: string;
    size_bytes: number;
    checksum_sha256: string;
  }): Promise<ApiResult<DeclaredUpload>>;
  put(url: string, contentType: string, file: File): Promise<boolean>;
  complete(attachmentId: string): Promise<ApiResult<AttachmentMeta>>;
}

// createUploadApi targets the requisition's BFF routes. declare/complete go
// through the BFF (httpOnly cookie auth); put goes directly to the presigned
// MinIO URL with the Content-Type the signature covers.
export function createUploadApi(requisitionId: string): UploadApi {
  return {
    async declare(declaration) {
      const res = await fetch(`/api/requisitions/${encodeURIComponent(requisitionId)}/attachments`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(declaration),
      });
      return parseApiResult(res);
    },
    async put(url, contentType, file) {
      const res = await fetch(url, {
        method: "PUT",
        headers: { "content-type": contentType },
        body: file,
      });
      return res.ok;
    },
    async complete(attachmentId) {
      const res = await fetch(
        `/api/requisitions/${encodeURIComponent(requisitionId)}/attachments/${encodeURIComponent(attachmentId)}/complete`,
        { method: "POST" },
      );
      return parseApiResult(res);
    },
  };
}

// parseApiResult decodes a BFF response into the ApiResult shape — exported
// since #41's invoice attachment panel builds its UploadApi on the same
// contract.
export async function parseApiResult<T>(res: Response): Promise<ApiResult<T>> {
  try {
    if (!res.ok) {
      const parsed = (await res.json().catch(() => null)) as { error?: string; code?: string } | null;
      return {
        ok: false,
        status: res.status,
        error: parsed?.error ?? `request failed (${res.status})`,
        code: parsed?.code ?? "gateway_error",
      };
    }
    return { ok: true, data: (await res.json()) as T };
  } catch {
    return { ok: false, status: 502, error: "network error", code: "gateway_unavailable" };
  }
}

// uploadAttachment runs declare → PUT → complete for one file. digest is
// injectable for tests; production uses sha256Hex.
export async function uploadAttachment(
  file: File,
  api: UploadApi,
  digest: (file: File) => Promise<string> = sha256Hex,
): Promise<UploadOutcome> {
  if (!ATTACHMENT_MIME_TYPES.includes(file.type as (typeof ATTACHMENT_MIME_TYPES)[number])) {
    return { ok: false, code: "upload_mime_unsupported", message: uploadErrorMessage("upload_mime_unsupported") };
  }
  if (file.size < 1) {
    return { ok: false, code: "upload_empty", message: uploadErrorMessage("upload_empty") };
  }
  const checksum = await digest(file);
  const declared = await api.declare({
    filename: file.name,
    mime_type: file.type,
    size_bytes: file.size,
    checksum_sha256: checksum,
  });
  if (!declared.ok) {
    return { ok: false, code: declared.code, message: uploadErrorMessage(declared.code) };
  }
  const uploaded = await api.put(
    declared.data.upload.url,
    declared.data.upload.headers["Content-Type"] ?? file.type,
    file,
  );
  if (!uploaded) {
    return { ok: false, code: "upload_put_failed", message: uploadErrorMessage("upload_put_failed") };
  }
  const completed = await api.complete(declared.data.id);
  if (!completed.ok) {
    return { ok: false, code: completed.code, message: uploadErrorMessage(completed.code) };
  }
  return { ok: true, attachment: completed.data };
}
