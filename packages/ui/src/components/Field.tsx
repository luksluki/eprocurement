import type { ReactNode } from "react";

type FieldBaseProps = {
  id: string;
  label: ReactNode;
  hint?: ReactNode;
  error?: ReactNode;
};

type FieldInputProps = FieldBaseProps & {
  control?: "input";
  name?: string;
  type?: "text" | "email" | "password" | "search" | "tel" | "url";
  value: string;
  onValueChange?: (value: string) => void;
  autoComplete?: string;
  placeholder?: string;
  required?: boolean;
  disabled?: boolean;
};

type FieldSelectProps = FieldBaseProps & {
  control: "select";
  name?: string;
  value: string;
  onValueChange?: (value: string) => void;
  options: ReadonlyArray<{ value: string; label: string }>;
  placeholderOption?: string;
  autoComplete?: string;
  required?: boolean;
  disabled?: boolean;
};

export type FieldProps = FieldInputProps | FieldSelectProps;

const CONTROL_CLASSES =
  "focus-ring mt-1 w-full rounded-md border border-line-strong bg-surface px-3 py-2 text-sm text-ink disabled:opacity-60";

// Label + control + hint/error block for forms. The control is an <input>
// (default) or a <select> via control="select"; both carry the focus-ring
// utility so keyboard focus is visible. Errors are announced via role=alert
// and linked with aria-describedby.
export function Field(props: FieldProps) {
  const { id, label, hint, error } = props;
  const hintId = hint ? `${id}-hint` : undefined;
  const errorId = error ? `${id}-error` : undefined;
  const describedBy = [hintId, errorId].filter(Boolean).join(" ") || undefined;

  return (
    <div>
      <label htmlFor={id} className="block text-sm font-medium text-ink-muted">
        {label}
      </label>
      {props.control === "select" ? (
        <select
          id={id}
          name={props.name}
          value={props.value}
          required={props.required}
          disabled={props.disabled}
          autoComplete={props.autoComplete}
          aria-describedby={describedBy}
          className={CONTROL_CLASSES}
          onChange={(event) => props.onValueChange?.(event.target.value)}
        >
          {props.placeholderOption !== undefined ? (
            <option value="" disabled>
              {props.placeholderOption}
            </option>
          ) : null}
          {props.options.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </select>
      ) : (
        <input
          id={id}
          name={props.name}
          type={props.type ?? "text"}
          value={props.value}
          required={props.required}
          disabled={props.disabled}
          autoComplete={props.autoComplete}
          placeholder={props.placeholder}
          aria-describedby={describedBy}
          className={CONTROL_CLASSES}
          onChange={(event) => props.onValueChange?.(event.target.value)}
        />
      )}
      {hint ? (
        <p id={hintId} className="mt-1 text-xs text-ink-subtle">
          {hint}
        </p>
      ) : null}
      {error ? (
        <p id={errorId} role="alert" className="mt-1 text-xs text-danger-fg">
          {error}
        </p>
      ) : null}
    </div>
  );
}
