import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { Button } from "./Button";

afterEach(() => {
  cleanup();
});

describe("Button", () => {
  it.each([
    { variant: "primary" as const, expected: "bg-brand" },
    { variant: "secondary" as const, expected: "border-line-strong" },
  ])("applies the $variant tone classes", ({ variant, expected }) => {
    render(
      <Button variant={variant} onClick={() => undefined}>
        Label
      </Button>,
    );
    const button = screen.getByRole("button", { name: "Label" });
    expect(button.className).toContain(expected);
  });

  it("carries the focus-ring utility so keyboard focus stays visible", () => {
    render(<Button>Label</Button>);
    expect(screen.getByRole("button").className).toContain("focus-ring");
  });

  it("defaults to type=button so plain usage never submits a form", () => {
    render(<Button>Label</Button>);
    expect(screen.getByRole("button").getAttribute("type")).toBe("button");
  });

  it("honours an explicit type=submit", () => {
    render(<Button type="submit">Label</Button>);
    expect(screen.getByRole("button").getAttribute("type")).toBe("submit");
  });

  it("invokes onClick when enabled", () => {
    const onClick = vi.fn();
    render(<Button onClick={onClick}>Label</Button>);
    fireEvent.click(screen.getByRole("button"));
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it("ignores clicks while disabled (failure path)", () => {
    const onClick = vi.fn();
    render(
      <Button onClick={onClick} disabled>
        Label
      </Button>,
    );
    fireEvent.click(screen.getByRole("button"));
    expect(onClick).not.toHaveBeenCalled();
    expect(screen.getByRole("button").className).toContain("disabled:opacity-60");
  });
});
