import { describe, expect, it } from "vitest";
import { safeNext } from "./redirect";

describe("safeNext open-redirect guard", () => {
  it("keeps same-origin absolute paths", () => {
    expect(safeNext("/app/vendors")).toBe("/app/vendors");
    expect(safeNext("/admin/settings?tab=rules")).toBe("/admin/settings?tab=rules");
  });

  it("falls back on absolute URLs and protocol-relative targets", () => {
    expect(safeNext("https://evil.example/phish")).toBe("/app/dashboard");
    expect(safeNext("//evil.example")).toBe("/app/dashboard");
    expect(safeNext("http://localhost:3000/app")).toBe("/app/dashboard");
  });

  it("falls back on backslash and scheme-separator tricks", () => {
    expect(safeNext("/\\evil.example")).toBe("/app/dashboard");
    expect(safeNext("/\\evil")).toBe("/app/dashboard");
    expect(safeNext("/redirect:https://evil.example")).toBe("/app/dashboard");
  });

  it("falls back on empty or missing values, honoring a custom default", () => {
    expect(safeNext(null)).toBe("/app/dashboard");
    expect(safeNext("")).toBe("/app/dashboard");
    expect(safeNext(undefined, "/app/vendors")).toBe("/app/vendors");
  });
});
