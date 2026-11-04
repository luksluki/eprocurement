import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import LoginForm from "./LoginForm";
import { getDictionary } from "@/i18n";

// The real English dictionary drives the labels — the test doubles as a
// smoke check that login copy resolves from the i18n module.
const labels = getDictionary("en").login;

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function submitForm() {
  fireEvent.change(screen.getByLabelText(labels.email), { target: { value: "admin@acme.example" } });
  fireEvent.change(screen.getByLabelText(labels.password), { target: { value: "wrong-password" } });
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

    render(<LoginForm labels={labels} />);
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

    render(<LoginForm labels={labels} />);
    submitForm();

    await waitFor(() => {
      expect(screen.getByRole("alert").textContent).toContain(labels.network);
    });
  });

  it("shows the switch-failure hint when arriving from a failed tenant switch", () => {
    render(<LoginForm labels={labels} switchFailed />);
    expect(screen.getByRole("alert").textContent).toContain(labels.switchFailed);
  });

  it("falls back to the dictionary's generic error when the body carries none", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(JSON.stringify({}), {
            status: 500,
            headers: { "content-type": "application/json" },
          }),
      ),
    );

    render(<LoginForm labels={labels} />);
    submitForm();

    await waitFor(() => {
      expect(screen.getByRole("alert").textContent).toContain(labels.invalid);
    });
  });
});
