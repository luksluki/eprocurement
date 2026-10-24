import type { ButtonHTMLAttributes, ReactNode } from "react";

export type ButtonVariant = "primary" | "secondary";
export type ButtonSize = "sm" | "md";

export type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: ButtonVariant;
  size?: ButtonSize;
  children: ReactNode;
};

const VARIANT_CLASSES: Record<ButtonVariant, string> = {
  primary: "bg-brand text-white hover:bg-brand-strong",
  secondary: "border border-line-strong bg-surface text-ink-muted hover:bg-canvas",
};

const SIZE_CLASSES: Record<ButtonSize, string> = {
  sm: "px-3 py-1 text-sm",
  md: "px-4 py-2 text-sm font-medium",
};

// The design-system button. Every variant carries the focus-ring utility so
// keyboard focus is always visible (PRD §19); copy always arrives via props
// (children) so the package stays presentational and i18n-neutral.
export function Button({
  variant = "primary",
  size = "md",
  type = "button",
  className,
  ...rest
}: ButtonProps) {
  const classes = [
    "focus-ring inline-flex items-center justify-center gap-2 rounded-md disabled:cursor-not-allowed disabled:opacity-60",
    VARIANT_CLASSES[variant],
    SIZE_CLASSES[size],
  ];
  if (className) {
    classes.push(className);
  }
  return (
    <button {...rest} type={type} className={classes.join(" ")}>
      {rest.children}
    </button>
  );
}
