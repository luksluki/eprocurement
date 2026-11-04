import { requireSession } from "@/lib/session";
import { getDictionary } from "@/i18n/server";
import RequisitionWizard from "./RequisitionWizard";

export const dynamic = "force-dynamic";

// PRD §11: /app/requisitions/new — the multi-step create form (details →
// items → attachments → review, issue #33). The server component guards the
// session and hands the locale's copy to the client wizard; everything else
// (RHF+Zod validation, draft autosave, presigned uploads) lives in
// RequisitionWizard.
export default async function NewRequisitionPage() {
  await requireSession("/app/requisitions/new");
  const dict = await getDictionary();

  return (
    <section aria-labelledby="new-requisition-title">
      <h1 id="new-requisition-title" className="text-2xl font-semibold">
        {dict.requisitionForm.title}
      </h1>
      <p className="mt-1 text-sm text-slate-500">{dict.requisitionForm.description}</p>
      <div className="mt-6">
        <RequisitionWizard copy={dict.requisitionForm} />
      </div>
    </section>
  );
}
