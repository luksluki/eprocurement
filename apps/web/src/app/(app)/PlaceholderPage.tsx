// Shared placeholder for sections of the PRD §11 information architecture
// whose screens arrive in later milestones — every sidebar link resolves to a
// real page; none 404s.
export default function PlaceholderPage({
  title,
  description,
  milestone,
}: {
  title: string;
  description: string;
  milestone: string;
}) {
  return (
    <section aria-labelledby="placeholder-title">
      <h1 id="placeholder-title" className="text-2xl font-semibold">
        {title}
      </h1>
      <p className="mt-2 max-w-2xl text-sm text-slate-600">{description}</p>
      <div className="mt-6 rounded-xl border border-dashed border-slate-300 bg-white px-6 py-10 text-center">
        <p className="text-sm font-medium text-slate-700">This screen is not built yet.</p>
        <p className="mt-1 text-xs text-slate-500">Planned in {milestone}.</p>
      </div>
    </section>
  );
}
