import { Card, EmptyState, PageHeader } from "@eprocurement/ui";
import { getDictionary } from "@/i18n/server";

// Shared placeholder for sections of the PRD §11 information architecture
// whose screens arrive in later milestones — every sidebar link resolves to a
// real page; none 404s. Structure and copy come from the design system and
// the i18n dictionary; the page-specific title/description/milestone are
// passed in (already localized) by each page.
export default async function PlaceholderPage({
  title,
  description,
  milestone,
}: {
  title: string;
  description: string;
  milestone: string;
}) {
  const dict = await getDictionary();
  return (
    <section aria-labelledby="placeholder-title">
      <PageHeader id="placeholder-title" title={title} description={description} />
      <Card variant="dashed" className="mt-6">
        <EmptyState title={dict.placeholder.notBuilt} body={dict.placeholder.plannedIn(milestone)} />
      </Card>
    </section>
  );
}
