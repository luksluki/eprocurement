"use client";

import { useState, type FormEvent } from "react";
import { Button, Card, ErrorState, Field } from "@eprocurement/ui";

export type LoginLabels = {
  heading: string;
  email: string;
  password: string;
  submit: string;
  submitting: string;
  invalid: string;
  network: string;
  switchFailed: string;
};

type Props = {
  labels: LoginLabels;
  initialEmail?: string;
  next?: string;
  switchFailed?: boolean;
};

// Login form on the design-system primitives (Card, Field, Button,
// ErrorState). Copy arrives via labels so the dictionary stays in the server
// layer (PRD §19 i18n).
export default function LoginForm({ labels, initialEmail = "", next = "", switchFailed = false }: Props) {
  const [email, setEmail] = useState(initialEmail);
  const [password, setPassword] = useState("");
  const [error, setError] = useState(switchFailed ? labels.switchFailed : "");
  const [busy, setBusy] = useState(false);

  async function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const res = await fetch("/api/auth/login", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ email, password, next }),
      });
      const data = (await res.json().catch(() => ({}))) as { error?: string; redirect?: string };
      if (!res.ok) {
        setError(data.error ?? labels.invalid);
        return;
      }
      // Full navigation so the new cookies apply to every request.
      window.location.assign(data.redirect ?? "/app/dashboard");
    } catch {
      setError(labels.network);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card className="p-6 shadow-sm">
      <form onSubmit={onSubmit} aria-labelledby="login-heading" noValidate={false}>
        <h2 id="login-heading" className="sr-only">
          {labels.heading}
        </h2>
        <Field
          id="email"
          label={labels.email}
          type="email"
          autoComplete="username"
          required
          value={email}
          onValueChange={setEmail}
        />
        <div className="mt-4">
          <Field
            id="password"
            label={labels.password}
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onValueChange={setPassword}
          />
        </div>
        {error ? (
          <div className="mt-4">
            <ErrorState message={error} />
          </div>
        ) : null}
        <Button type="submit" disabled={busy} className="mt-6 w-full">
          {busy ? labels.submitting : labels.submit}
        </Button>
      </form>
    </Card>
  );
}
