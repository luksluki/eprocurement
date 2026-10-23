import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import LanguageSwitcher from "./LanguageSwitcher";
import { getDictionary } from "@/i18n";

const refresh = vi.fn();

vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh, push: vi.fn(), replace: vi.fn() }),
}));

beforeEach(() => {
  refresh.mockClear();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function names() {
  return { id: getDictionary("id").localeNames.id, en: getDictionary("en").localeNames.en };
}

describe("LanguageSwitcher", () => {
  it("marks the active locale with aria-pressed", () => {
    render(<LanguageSwitcher locale="id" names={names()} />);
    expect(screen.getByRole("button", { name: "Bahasa Indonesia" }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByRole("button", { name: "English" }).getAttribute("aria-pressed")).toBe("false");
  });

  it("POSTs the new locale and refreshes the server tree", async () => {
    const fetchMock = vi.fn(async () => new Response(JSON.stringify({ ok: true }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    render(<LanguageSwitcher locale="id" names={names()} />);
    fireEvent.click(screen.getByRole("button", { name: "English" }));

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        "/api/locale",
        expect.objectContaining({
          method: "POST",
          body: JSON.stringify({ locale: "en" }),
        }),
      );
      expect(refresh).toHaveBeenCalledTimes(1);
    });
  });

  it("keeps the current locale when the POST fails (failure path)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("fetch failed");
      }),
    );

    render(<LanguageSwitcher locale="en" names={names()} />);
    fireEvent.click(screen.getByRole("button", { name: "Bahasa Indonesia" }));

    await waitFor(() => {
      expect(refresh).toHaveBeenCalledTimes(1);
    });
    // No cookie was written; the rendered active locale is unchanged.
    expect(screen.getByRole("button", { name: "English" }).getAttribute("aria-pressed")).toBe("true");
  });

  it("ignores a click on the already-active locale", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    render(<LanguageSwitcher locale="id" names={names()} />);
    fireEvent.click(screen.getByRole("button", { name: "Bahasa Indonesia" }));
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(fetchMock).not.toHaveBeenCalled();
    expect(refresh).not.toHaveBeenCalled();
  });
});
