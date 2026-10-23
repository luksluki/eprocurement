import PlaceholderPage from "../../PlaceholderPage";
import { getDictionary } from "@/i18n/server";

export const dynamic = "force-dynamic";

export default async function InvoicesPage() {
  const p = (await getDictionary()).pages.invoices;
  return <PlaceholderPage title={p.title} description={p.description} milestone={p.milestone} />;
}
