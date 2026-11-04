import { describe, expect, it } from "vitest";
import {
  detailsStepSchema,
  itemsAreValid,
  itemsStepSchema,
  requisitionFormSchema,
  type ItemFormValues,
} from "./schema";

const validItem: ItemFormValues = {
  description: "Laptop",
  qty: 2,
  unit_price_cents: 7_500_000,
  category: "it",
};

describe("requisition wizard schema (domain mirror, #33)", () => {
  it("accepts the happy path and both currencies", () => {
    for (const currency of ["IDR", "USD"] as const) {
      const result = requisitionFormSchema.safeParse({ currency, items: [validItem] });
      expect(result.success).toBe(true);
    }
  });

  it("rejects an unchosen currency (no default — the create API contract)", () => {
    expect(detailsStepSchema.safeParse({ currency: "" }).success).toBe(false);
    expect(detailsStepSchema.safeParse({ currency: "EUR" }).success).toBe(false);
    expect(detailsStepSchema.safeParse({ currency: "idr" }).success).toBe(false); // case-sensitive client-side
  });

  it("rejects invalid items per the domain rules", () => {
    const cases: Array<[Partial<ItemFormValues>, string]> = [
      [{ description: "   " }, "blank description"],
      [{ qty: 0 }, "qty zero"],
      [{ qty: -1 }, "negative qty"],
      [{ qty: 1.5 }, "fractional qty"],
      [{ unit_price_cents: -1 }, "negative price"],
    ];
    for (const [mutate] of cases) {
      const item = { ...validItem, ...mutate };
      expect(requisitionFormSchema.safeParse({ currency: "IDR", items: [item] }).success).toBe(false);
    }
  });

  it("accepts price zero (unit_price_cents >= 0)", () => {
    const result = requisitionFormSchema.safeParse({
      currency: "IDR",
      items: [{ ...validItem, unit_price_cents: 0 }],
    });
    expect(result.success).toBe(true);
  });

  it("allows an empty basket for a draft (items land via autosave)", () => {
    expect(requisitionFormSchema.safeParse({ currency: "IDR", items: [] }).success).toBe(true);
    expect(itemsAreValid([])).toBe(true);
  });

  it("itemsAreValid mirrors itemsStepSchema for partial edits", () => {
    expect(itemsAreValid([validItem])).toBe(true);
    expect(itemsAreValid([{ ...validItem, qty: Number.NaN }])).toBe(false);
    expect(itemsStepSchema.safeParse({ items: [{ ...validItem, description: "" }] }).success).toBe(false);
  });
});
