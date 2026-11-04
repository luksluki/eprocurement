import type { ReactNode } from "react";

export type BadgeTone = "neutral" | "success" | "danger";

export type BadgeProps = {
  tone?: BadgeTone;
  children: ReactNode;
};

const TONE_CLASSES: Record<BadgeTone, string> = {
  neutral: "bg-canvas text-ink-muted",
  success: "bg-success-bg text-success-fg",
  danger: "bg-danger-bg text-danger-fg",
};

// Compact status pill. Tones are token pairs whose contrast is recorded in
// dev note 051.
export function Badge({ tone = "neutral", children }: BadgeProps) {
  return (
    <span
      className={`inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ${TONE_CLASSES[tone]}`}
    >
      {children}
    </span>
  );
}
