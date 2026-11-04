import type { ReactNode } from "react";

export type CardVariant = "solid" | "dashed";

export type CardProps = {
  variant?: CardVariant;
  className?: string;
  children: ReactNode;
};

const VARIANT_CLASSES: Record<CardVariant, string> = {
  solid: "border border-line",
  dashed: "border border-dashed border-line-strong",
};

// The base surface container: white background, rounded corners, border.
// "dashed" marks not-yet-built placeholder panels.
export function Card({ variant = "solid", className, children }: CardProps) {
  const classes = ["rounded-xl bg-surface", VARIANT_CLASSES[variant]];
  if (className) {
    classes.push(className);
  }
  return <div className={classes.join(" ")}>{children}</div>;
}
