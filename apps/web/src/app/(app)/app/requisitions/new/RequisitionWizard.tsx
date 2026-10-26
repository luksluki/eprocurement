"use client";

// RequisitionWizard (#33): the PRD §11 four-step create form — details →
// items → attachments → review. React Hook Form + Zod mirror the domain
// rules (currency ∈ {IDR, USD}, non-blank descriptions, qty > 0,
// unit_price_cents >= 0, ≥1 item to submit); a draft is created on the
// server as soon as the details step is valid, the basket autosaves into it,
// and the URL carries ?draft=<id> so a reload restores header, items, and
// attachments. Attachments upload declare → presigned PUT (direct to MinIO)
// → complete; machine error codes map to per-step messages.
import { useCallback, useEffect, useMemo, useState } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useFieldArray, useForm, useWatch } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import type { AttachmentMeta, RequisitionDetail, RequisitionItemWire } from "@/lib/gateway";
import { callApi } from "@/lib/api-client";
import {
  CURRENCIES,
  itemsAreValid,
  requisitionFormSchema,
  type RequisitionFormValues,
} from "./schema";
import { useAutosave, type AutosaveOutcome } from "./useAutosave";
import { createUploadApi, uploadAttachment } from "./upload";

export const WIZARD_STEPS = ["details", "items", "attachments", "review"] as const;

// Copy flows in from the server page (dictionaries stay server-side per the
// i18n convention; client components take strings as props).
export type WizardCopy = {
  steps: { details: string; items: string; attachments: string; review: string };
  details: { heading: string; currencyLabel: string; currencyPlaceholder: string; currencyLockedNote: string; next: string };
  items: {
    heading: string;
    description: string;
    qty: string;
    unitPrice: string;
    category: string;
    add: string;
    remove: string;
    next: string;
    back: string;
  };
  attachments: { heading: string; hint: string; add: string; empty: string; uploading: string; next: string; back: string };
  review: {
    heading: string;
    itemsHeading: string;
    attachmentsHeading: string;
    total: string;
    submit: string;
    submitting: string;
    back: string;
    itemsEmpty: string;
  };
  autosave: { saving: string; saved: string; failed: string };
  restore: { loading: string; notFound: string; error: string; notDraft: string };
  submitted: { heading: string; backToList: string };
};

const submitErrorMessages: Record<string, string> = {
  requisition_items_empty: "Add at least one item before submitting.",
  requisition_illegal_transition: "This requisition is no longer a draft — it may have been submitted already.",
  approval_rules_missing: "No approval rules are configured for this currency yet — ask a tenant admin to add them.",
  requisition_not_found: "This draft no longer exists.",
  requisition_forbidden: "Only the requester may submit this requisition.",
  gateway_unavailable: "The gateway is unreachable — check your connection and retry.",
};

function emptyItem(): RequisitionFormValues["items"][number] {
  return { description: "", qty: 1, unit_price_cents: 0, category: "" };
}

function toWire(items: RequisitionFormValues["items"]): RequisitionItemWire[] {
  return items.map((it) => ({
    description: it.description,
    qty: Number(it.qty),
    unit_price_cents: Number(it.unit_price_cents),
    category: it.category,
  }));
}

export default function RequisitionWizard({ copy }: { copy: WizardCopy }) {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const draftIdParam = searchParams.get("draft");

  const [step, setStep] = useState<(typeof WIZARD_STEPS)[number]>("details");
  const [draftId, setDraftId] = useState<string | null>(draftIdParam);
  const [attachments, setAttachments] = useState<AttachmentMeta[]>([]);
  const [uploading, setUploading] = useState(false);
  const [uploadError, setUploadError] = useState<{ code: string; message: string } | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<{ code: string; message: string } | null>(null);
  const [submittedNumber, setSubmittedNumber] = useState<string | null>(null);
  const [restore, setRestore] = useState<{
    status: "idle" | "loading" | "ready" | "error";
    message?: string;
    code?: string;
    readOnly?: boolean;
  }>({ status: draftIdParam ? "loading" : "idle" });

  const form = useForm<RequisitionFormValues>({
    resolver: zodResolver(requisitionFormSchema),
    mode: "onTouched",
    // currency starts empty ("" casts into the enum's type): the domain has
    // no default currency and the details step gates on a real choice.
    defaultValues: { currency: "" as RequisitionFormValues["currency"], items: [emptyItem()] },
  });
  const { fields, append, remove } = useFieldArray({ control: form.control, name: "items" });

  const watchedCurrency = useWatch({ control: form.control, name: "currency" });
  const watchedItems = useWatch({ control: form.control, name: "items" }) ?? [];
  const detailsValid = CURRENCIES.includes(watchedCurrency as (typeof CURRENCIES)[number]);
  const itemsValid = itemsAreValid(watchedItems);
  const readOnly = restore.readOnly === true || submittedNumber !== null;

  // Autosave: create the draft once the currency is chosen, then PUT the
  // basket on every valid change. 403/409 stop it permanently (AC).
  const createDraft = useCallback(async (currency: string): Promise<AutosaveOutcome> => {
    const res = await callApi<RequisitionDetail>("/api/requisitions", { method: "POST", body: { currency } });
    return res.ok ? { ok: true, id: res.data.id } : { ok: false, status: res.status, error: res.error, code: res.code };
  }, []);
  const saveItems = useCallback(async (id: string, items: RequisitionItemWire[]): Promise<AutosaveOutcome> => {
    const res = await callApi<RequisitionDetail>(`/api/requisitions/${encodeURIComponent(id)}`, {
      method: "PUT",
      body: { items },
    });
    return res.ok ? { ok: true } : { ok: false, status: res.status, error: res.error, code: res.code };
  }, []);
  const onDraftCreated = useCallback(
    (id: string) => {
      setDraftId(id);
      router.replace(`${pathname}?draft=${encodeURIComponent(id)}`, { scroll: false });
    },
    [router, pathname],
  );
  const autosave = useAutosave({
    currency: watchedCurrency,
    detailsValid,
    itemsValid,
    draftId,
    items: toWire(watchedItems),
    createDraft,
    saveItems,
    onDraftCreated,
  });

  // Restore from ?draft=<id>: header, items, and the embedded attachments.
  // Non-draft statuses flip the form read-only (a submitted requisition is
  // immutable; autosave would 409 on every keystroke).
  const seedRef = autosave.seed;
  useEffect(() => {
    if (!draftIdParam) {
      return;
    }
    let cancelled = false;
    void (async () => {
      const res = await callApi<RequisitionDetail>(`/api/requisitions/${encodeURIComponent(draftIdParam)}`);
      if (cancelled) {
        return;
      }
      if (!res.ok) {
        setRestore({
          status: "error",
          message:
            res.status === 404
              ? copy.restore.notFound
              : res.status === 403
                ? "This draft belongs to another user."
                : copy.restore.error,
          code: res.code,
        });
        return;
      }
      const detail = res.data;
      form.reset({
        currency: detail.currency as RequisitionFormValues["currency"],
        items: detail.items.length > 0 ? detail.items.map((it) => ({ ...it })) : [emptyItem()],
      });
      setAttachments(detail.attachments ?? []);
      seedRef(toWire(detail.items));
      if (detail.status !== "draft") {
        setRestore({ status: "ready", readOnly: true, message: copy.restore.notDraft });
      } else {
        setRestore({ status: "ready" });
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- run once per ?draft param
  }, [draftIdParam]);

  const uploadApi = useMemo(() => (draftId ? createUploadApi(draftId) : null), [draftId]);

  const onFileChosen = async (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    event.target.value = "";
    if (!file || !uploadApi) {
      return;
    }
    setUploading(true);
    setUploadError(null);
    const outcome = await uploadAttachment(file, uploadApi);
    if (outcome.ok) {
      setAttachments((list) => [...list, outcome.attachment]);
    } else {
      setUploadError(outcome);
    }
    setUploading(false);
  };

  const goItems = async () => {
    if (await form.trigger("currency")) {
      setStep("items");
    }
  };
  const goAttachments = async () => {
    if (await form.trigger("items")) {
      setStep("attachments");
    }
  };

  const onSubmit = form.handleSubmit(async (values) => {
    if (values.items.length === 0) {
      setSubmitError({ code: "requisition_items_empty", message: copy.review.itemsEmpty });
      return;
    }
    if (!draftId) {
      setSubmitError({ code: "draft_missing", message: "The draft has not been created yet — go back to the first step." });
      return;
    }
    setSubmitting(true);
    setSubmitError(null);
    const res = await callApi<RequisitionDetail>(`/api/requisitions/${encodeURIComponent(draftId)}/submit`, {
      method: "POST",
      headers: { "idempotency-key": crypto.randomUUID() },
    });
    setSubmitting(false);
    if (res.ok) {
      setSubmittedNumber(res.data.number);
      return;
    }
    setSubmitError({
      code: res.code,
      message: submitErrorMessages[res.code] ?? res.error,
    });
  });

  const totalCents = toWire(form.getValues("items") ?? []).reduce(
    (sum, it) => (Number.isFinite(it.qty) && Number.isFinite(it.unit_price_cents) ? sum + it.qty * it.unit_price_cents : sum),
    0,
  );

  const stepIndex = WIZARD_STEPS.indexOf(step);

  return (
    <section aria-label="New requisition">
      <ol className="mb-6 flex flex-wrap items-center gap-2 text-sm" data-testid="wizard-steps">
        {WIZARD_STEPS.map((name, index) => (
          <li
            key={name}
            className={`rounded-full px-3 py-1 font-medium ${
              index === stepIndex
                ? "bg-slate-900 text-white"
                : index < stepIndex
                  ? "bg-emerald-50 text-emerald-700"
                  : "bg-slate-100 text-slate-500"
            }`}
            data-testid={`wizard-step-${name}`}
          >
            {index + 1}. {copy.steps[name]}
          </li>
        ))}
      </ol>

      {/* Autosave status: saved/failed states are visible; a stopped
          autosave (foreign or non-draft requisition) shows its message. */}
      <p
        className="mb-4 text-xs text-slate-500"
        data-testid="autosave-status"
        role="status"
      >
        {autosave.stopped
          ? autosave.message
          : autosave.status === "saving"
            ? copy.autosave.saving
            : autosave.status === "saved"
              ? copy.autosave.saved
              : autosave.status === "error"
                ? `${copy.autosave.failed} ${autosave.message ?? ""}`
                : ""}
      </p>

      {restore.status === "loading" ? (
        <p className="text-sm text-slate-500" data-testid="restore-loading">
          {copy.restore.loading}
        </p>
      ) : restore.status === "error" ? (
        <div className="rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700" role="alert" data-testid="restore-error">
          <p>{restore.message}</p>
          {restore.code ? <p className="mt-1 font-mono text-xs">{restore.code}</p> : null}
          <a className="mt-2 inline-block underline" href="/app/requisitions/new">
            Start a new requisition
          </a>
        </div>
      ) : submittedNumber ? (
        <div className="rounded-xl border border-emerald-200 bg-emerald-50 px-4 py-3 text-sm text-emerald-800" data-testid="submitted">
          <p>
            {copy.submitted.heading} <span className="font-mono">{submittedNumber}</span>
          </p>
          <a className="mt-2 inline-block underline" href="/app/requisitions">
            {copy.submitted.backToList}
          </a>
        </div>
      ) : (
        <form onSubmit={onSubmit} noValidate>
          {readOnly && restore.message ? (
            <div className="mb-4 rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-800" role="status" data-testid="readonly-notice">
              {restore.message}
            </div>
          ) : null}

          {step === "details" ? (
            <fieldset className="rounded-xl border border-slate-200 bg-white p-6" data-testid="step-panel-details">
              <legend className="px-1 text-sm font-medium text-slate-700">{copy.details.heading}</legend>
              <label className="flex max-w-xs flex-col gap-1 text-xs font-medium text-slate-500">
                {copy.details.currencyLabel}
                <select
                  {...form.register("currency")}
                  disabled={!!draftId || readOnly}
                  className="rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-900 disabled:bg-slate-50 disabled:text-slate-500"
                  data-testid="input-currency"
                >
                  <option value="">{copy.details.currencyPlaceholder}</option>
                  {CURRENCIES.map((c) => (
                    <option key={c} value={c}>
                      {c}
                    </option>
                  ))}
                </select>
              </label>
              {form.formState.errors.currency ? (
                <p className="mt-2 text-xs text-red-600" data-testid="field-error-currency">
                  {form.formState.errors.currency.message}
                </p>
              ) : null}
              {draftId ? <p className="mt-2 text-xs text-slate-400">{copy.details.currencyLockedNote}</p> : null}
              <div className="mt-4">
                <button
                  type="button"
                  onClick={() => void goItems()}
                  disabled={!detailsValid || readOnly}
                  className="rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-700 disabled:cursor-not-allowed disabled:opacity-40"
                  data-testid="next-from-details"
                >
                  {copy.details.next}
                </button>
              </div>
            </fieldset>
          ) : null}

          {step === "items" ? (
            <fieldset className="rounded-xl border border-slate-200 bg-white p-6" data-testid="step-panel-items">
              <legend className="px-1 text-sm font-medium text-slate-700">{copy.items.heading}</legend>
              <div className="flex flex-col gap-4">
                {fields.map((field, index) => {
                  const errors = form.formState.errors.items?.[index];
                  return (
                    <div key={field.id} className="grid grid-cols-1 gap-3 rounded-lg border border-slate-100 p-3 md:grid-cols-12" data-testid={`item-row-${index}`}>
                      <label className="flex flex-col gap-1 text-xs font-medium text-slate-500 md:col-span-4">
                        {copy.items.description}
                        <input
                          {...form.register(`items.${index}.description` as const)}
                          disabled={readOnly}
                          className="rounded-lg border border-slate-200 px-3 py-2 text-sm text-slate-900"
                          data-testid={`item-description-${index}`}
                        />
                        {errors?.description ? (
                          <span className="text-xs text-red-600" data-testid={`item-error-description-${index}`}>
                            {errors.description.message}
                          </span>
                        ) : null}
                      </label>
                      <label className="flex flex-col gap-1 text-xs font-medium text-slate-500 md:col-span-2">
                        {copy.items.qty}
                        <input
                          type="number"
                          {...form.register(`items.${index}.qty` as const, { valueAsNumber: true })}
                          disabled={readOnly}
                          className="rounded-lg border border-slate-200 px-3 py-2 text-sm text-slate-900"
                          data-testid={`item-qty-${index}`}
                        />
                        {errors?.qty ? (
                          <span className="text-xs text-red-600" data-testid={`item-error-qty-${index}`}>
                            {errors.qty.message}
                          </span>
                        ) : null}
                      </label>
                      <label className="flex flex-col gap-1 text-xs font-medium text-slate-500 md:col-span-3">
                        {copy.items.unitPrice}
                        <input
                          type="number"
                          {...form.register(`items.${index}.unit_price_cents` as const, { valueAsNumber: true })}
                          disabled={readOnly}
                          className="rounded-lg border border-slate-200 px-3 py-2 text-sm text-slate-900"
                          data-testid={`item-unit-price-${index}`}
                        />
                        {errors?.unit_price_cents ? (
                          <span className="text-xs text-red-600" data-testid={`item-error-unit-price-${index}`}>
                            {errors.unit_price_cents.message}
                          </span>
                        ) : null}
                      </label>
                      <label className="flex flex-col gap-1 text-xs font-medium text-slate-500 md:col-span-2">
                        {copy.items.category}
                        <input
                          {...form.register(`items.${index}.category` as const)}
                          disabled={readOnly}
                          className="rounded-lg border border-slate-200 px-3 py-2 text-sm text-slate-900"
                          data-testid={`item-category-${index}`}
                        />
                      </label>
                      <div className="flex items-end md:col-span-1">
                        <button
                          type="button"
                          onClick={() => remove(index)}
                          disabled={readOnly}
                          className="rounded-lg border border-slate-300 px-2 py-1.5 text-xs font-medium text-slate-600 hover:bg-slate-50 disabled:opacity-40"
                          data-testid={`item-remove-${index}`}
                          aria-label={copy.items.remove}
                        >
                          ×
                        </button>
                      </div>
                    </div>
                  );
                })}
              </div>
              <div className="mt-4 flex flex-wrap gap-2">
                <button
                  type="button"
                  onClick={() => append(emptyItem())}
                  disabled={readOnly}
                  className="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50 disabled:opacity-40"
                  data-testid="item-add"
                >
                  {copy.items.add}
                </button>
                <button
                  type="button"
                  onClick={() => setStep("details")}
                  className="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50"
                  data-testid="back-from-items"
                >
                  {copy.items.back}
                </button>
                <button
                  type="button"
                  onClick={() => void goAttachments()}
                  className="rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-700 disabled:cursor-not-allowed disabled:opacity-40"
                  data-testid="next-from-items"
                >
                  {copy.items.next}
                </button>
              </div>
            </fieldset>
          ) : null}

          {step === "attachments" ? (
            <fieldset className="rounded-xl border border-slate-200 bg-white p-6" data-testid="step-panel-attachments">
              <legend className="px-1 text-sm font-medium text-slate-700">{copy.attachments.heading}</legend>
              <p className="text-xs text-slate-500">{copy.attachments.hint}</p>
              <div className="mt-3 flex flex-wrap items-center gap-3">
                <input
                  type="file"
                  onChange={(e) => void onFileChosen(e)}
                  disabled={!draftId || uploading || readOnly}
                  accept="application/pdf,image/png,image/jpeg,image/webp,.docx,.xlsx"
                  className="text-sm"
                  data-testid="attachment-input"
                />
                {uploading ? (
                  <span className="text-xs text-slate-500" data-testid="uploading">
                    {copy.attachments.uploading}
                  </span>
                ) : null}
              </div>
              {uploadError ? (
                <p className="mt-2 text-xs text-red-600" role="alert" data-testid="upload-error">
                  {uploadError.message} <span className="font-mono">({uploadError.code})</span>
                </p>
              ) : null}
              <ul className="mt-4 flex flex-col gap-2" data-testid="attachment-list">
                {attachments.length === 0 ? (
                  <li className="text-sm text-slate-500">{copy.attachments.empty}</li>
                ) : (
                  attachments.map((att) => (
                    <li key={att.id} className="flex items-center justify-between rounded-lg border border-slate-100 px-3 py-2 text-sm" data-testid={`attachment-${att.id}`}>
                      <span className="truncate">
                        {att.filename} <span className="text-xs text-slate-400">({Math.ceil(att.size_bytes / 1024)} KB)</span>
                      </span>
                      <span
                        className={`rounded-full px-2 py-0.5 text-xs font-medium ${
                          att.status === "ready" ? "bg-emerald-50 text-emerald-700" : "bg-amber-50 text-amber-700"
                        }`}
                      >
                        {att.status}
                      </span>
                    </li>
                  ))
                )}
              </ul>
              <div className="mt-4 flex gap-2">
                <button
                  type="button"
                  onClick={() => setStep("items")}
                  className="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50"
                  data-testid="back-from-attachments"
                >
                  {copy.attachments.back}
                </button>
                <button
                  type="button"
                  onClick={() => setStep("review")}
                  className="rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-700"
                  data-testid="next-from-attachments"
                >
                  {copy.attachments.next}
                </button>
              </div>
            </fieldset>
          ) : null}

          {step === "review" ? (
            <fieldset className="rounded-xl border border-slate-200 bg-white p-6" data-testid="step-panel-review">
              <legend className="px-1 text-sm font-medium text-slate-700">{copy.review.heading}</legend>
              <dl className="grid grid-cols-2 gap-2 text-sm">
                <dt className="text-slate-500">Currency</dt>
                <dd>{form.getValues("currency")}</dd>
              </dl>
              <h3 className="mt-4 text-xs font-semibold uppercase tracking-wide text-slate-400">{copy.review.itemsHeading}</h3>
              <table className="mt-2 w-full text-left text-sm" data-testid="review-items">
                <thead className="text-xs uppercase tracking-wide text-slate-400">
                  <tr>
                    <th className="py-2">{copy.items.description}</th>
                    <th className="py-2">{copy.items.qty}</th>
                    <th className="py-2">{copy.items.unitPrice}</th>
                    <th className="py-2">{copy.items.category}</th>
                  </tr>
                </thead>
                <tbody>
                  {(form.getValues("items") ?? []).map((it, index) => (
                    <tr key={index} className="border-t border-slate-100">
                      <td className="py-2">{it.description}</td>
                      <td className="py-2">{it.qty}</td>
                      <td className="py-2">{it.unit_price_cents}</td>
                      <td className="py-2">{it.category}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              <p className="mt-2 text-sm font-medium" data-testid="review-total">
                {copy.review.total}: {totalCents.toLocaleString("en-US")} {form.getValues("currency")}
              </p>
              <h3 className="mt-4 text-xs font-semibold uppercase tracking-wide text-slate-400">{copy.review.attachmentsHeading}</h3>
              <p className="text-sm text-slate-600" data-testid="review-attachments-count">
                {attachments.length}
              </p>
              {submitError ? (
                <p className="mt-3 text-sm text-red-600" role="alert" data-testid="submit-error">
                  {submitError.message} <span className="font-mono text-xs">({submitError.code})</span>
                </p>
              ) : null}
              <div className="mt-4 flex gap-2">
                <button
                  type="button"
                  onClick={() => setStep("attachments")}
                  className="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50"
                  data-testid="back-from-review"
                >
                  {copy.review.back}
                </button>
                <button
                  type="submit"
                  disabled={submitting || readOnly || !draftId}
                  className="rounded-lg bg-emerald-600 px-4 py-2 text-sm font-medium text-white hover:bg-emerald-500 disabled:cursor-not-allowed disabled:opacity-40"
                  data-testid="submit-requisition"
                >
                  {submitting ? copy.review.submitting : copy.review.submit}
                </button>
              </div>
            </fieldset>
          ) : null}
        </form>
      )}
    </section>
  );
}
