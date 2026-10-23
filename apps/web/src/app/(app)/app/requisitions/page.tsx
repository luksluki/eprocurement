import PlaceholderPage from "../../PlaceholderPage";

export const dynamic = "force-dynamic";

export default function RequisitionsPage() {
  return (
    <PlaceholderPage
      title="Requisitions"
      description="Purchase requisition list with filters and pagination, plus the multi-step create form (details → items → attachments → review)."
      milestone="M2 — PR CRUD & approval engine"
    />
  );
}
