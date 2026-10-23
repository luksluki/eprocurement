import PlaceholderPage from "../../PlaceholderPage";

export const dynamic = "force-dynamic";

export default function PurchaseOrdersPage() {
  return (
    <PlaceholderPage
      title="Purchase Orders"
      description="PO list and detail with issue/acknowledge actions and goods receipts (partial receipts supported, over-receipt rejected)."
      milestone="M3 — PO & GRN"
    />
  );
}
