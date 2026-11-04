import type { ReactNode } from "react";

export type PageHeaderProps = {
  id: string;
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
};

// Standard page heading block. Pages pass an explicit id so the section can
// reference it via aria-labelledby.
export function PageHeader({ id, title, description, actions }: PageHeaderProps) {
  return (
    <div className="flex flex-wrap items-start justify-between gap-4">
      <div className="min-w-0">
        <h1 id={id} className="text-2xl font-semibold text-ink">
          {title}
        </h1>
        {description ? <p className="mt-1 text-sm text-ink-subtle">{description}</p> : null}
      </div>
      {actions ? <div className="flex items-center gap-2">{actions}</div> : null}
    </div>
  );
}
