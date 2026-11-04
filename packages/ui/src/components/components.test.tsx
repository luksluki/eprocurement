import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { Badge } from "./Badge";
import { Card } from "./Card";
import { EmptyState } from "./EmptyState";
import { ErrorState } from "./ErrorState";
import { PageHeader } from "./PageHeader";

afterEach(() => {
  cleanup();
});

describe("Badge", () => {
  it.each([
    { tone: "success" as const, expected: "bg-success-bg" },
    { tone: "danger" as const, expected: "bg-danger-bg" },
    { tone: "neutral" as const, expected: "bg-canvas" },
  ])("applies the $tone token pair", ({ tone, expected }) => {
    render(<Badge tone={tone}>active</Badge>);
    expect(screen.getByText("active").className).toContain(expected);
  });
});

describe("Card", () => {
  it("renders the solid variant by default", () => {
    render(<Card>content</Card>);
    expect(screen.getByText("content").className).toContain("border-line");
    expect(screen.getByText("content").className).not.toContain("border-dashed");
  });

  it("renders the dashed variant on demand", () => {
    render(<Card variant="dashed">content</Card>);
    expect(screen.getByText("content").className).toContain("border-dashed");
  });
});

describe("PageHeader", () => {
  it("renders title and description under a stable id", () => {
    render(<PageHeader id="vendors-title" title="Vendors" description="Tenant-scoped master data" />);
    expect(screen.getByText("Vendors").id).toBe("vendors-title");
    expect(screen.getByText("Tenant-scoped master data").tagName).toBe("P");
  });

  it("omits the description and actions when not provided", () => {
    render(<PageHeader id="t" title="Only title" />);
    expect(screen.getByRole("heading", { name: "Only title", level: 1 })).toBeTruthy();
  });
});

describe("EmptyState", () => {
  it("renders title and body", () => {
    render(<EmptyState title="No vendors yet." body="Seed data to see rows." />);
    expect(screen.getByText("No vendors yet.").tagName).toBe("P");
    expect(screen.getByText("Seed data to see rows.").tagName).toBe("P");
  });
});

describe("ErrorState", () => {
  it("announces the message with role=alert (failure path)", () => {
    render(<ErrorState message="Failed to load vendors." />);
    expect(screen.getByRole("alert").textContent).toBe("Failed to load vendors.");
  });
});
