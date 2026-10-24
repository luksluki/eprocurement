import PlaceholderPage from "../../PlaceholderPage";
import { getDictionary } from "@/i18n/server";

export const dynamic = "force-dynamic";

export default async function PurchaseOrdersPage() {
  const p = (await getDictionary()).pages.purchaseOrders;
  return <PlaceholderPage title={p.title} description={p.description} milestone={p.milestone} />;
}
