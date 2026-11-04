import type { ReactNode } from "react";

export type ErrorStateProps = {
  message: ReactNode;
};

// Inline failure notice for failed loads/submits. role="alert" announces it
// to assistive technology; the token pair (danger-bg/danger-fg) is AA-checked
// in dev note 051.
export function ErrorState({ message }: ErrorStateProps) {
  return (
    <div role="alert" className="rounded-md bg-danger-bg px-3 py-2 text-sm text-danger-fg">
      {message}
    </div>
  );
}
