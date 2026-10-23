import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import LoginForm from "./LoginForm";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function submitForm() {
  fireEvent.change(screen.getByLabelText("Email"), { target: { value: "admin@acme.example" } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "wrong-password" } });
  fireEvent.click(screen.getByRole("button", { name: /sign in/i }));
}

describe("LoginForm", () => {
  it("shows the gateway error when credentials are rejected (401)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(JSON.stringify({ error: "Invalid email or password." }), {
            status: 401,
            headers: { "content-type": "application/json" },
          }),
      ),
    );

    render(<LoginForm />);
    submitForm();

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Invalid email or password.");
  });

  it("shows a network error when the BFF call throws", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("fetch failed");
      }),
    );

    render(<LoginForm />);
    submitForm();

    await waitFor(() => {
      expect(screen.getByRole("alert").textContent).toContain("Network error");
    });
  });

  it("shows the switch-failure hint when arriving from a failed tenant switch", () => {
    render(<LoginForm switchFailed />);
    expect(screen.getByRole("alert").textContent).toContain("Tenant switch failed.");
  });
});
