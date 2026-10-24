import type { ReactNode } from "react";

export type EmptyStateProps = {
  title: ReactNode;
  body?: ReactNode;
};

// Centered "nothing here yet" block rendered inside a Card (typically the
// table container or a placeholder panel).
export function EmptyState({ title, body }: EmptyStateProps) {
  return (
    <div className="px-6 py-10 text-center">
      <p className="text-sm font-medium text-ink-muted">{title}</p>
      {body ? <p className="mt-1 text-xs text-ink-subtle">{body}</p> : null}
    </div>
  );
}
