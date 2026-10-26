// Zod schemas for the requisition wizard (#33) — the client mirror of the
// domain rules enforced by procurement (services/procurement/requisition/
// requisition.go): currency ∈ {IDR, USD} with no default, every item needs
// a non-blank description, qty > 0, unit_price_cents >= 0, and submit
// requires at least one item. Steps validate against slices of the same
// schema so the rules cannot drift between steps.
import { z } from "zod";

export const CURRENCIES = ["IDR", "USD"] as const;
export type Currency = (typeof CURRENCIES)[number];

// "" is the wizard's initial state (the user must choose); keep it in the
// form type but outside the enum so validation fails until a choice is made
// — the same contract CreateRequisition enforces (no default currency).
export const currencySchema = z.enum(CURRENCIES, { message: "Choose a currency" });

export const itemSchema = z.object({
  description: z.string().trim().min(1, "Description must not be blank"),
  // Numbers arrive via RHF's valueAsNumber (never coerced here) so the zod
  // input and output types match and the resolver types line up.
  qty: z.number({ message: "Qty must be a number" })
    .int("Qty must be a whole number")
    .positive("Qty must be greater than zero"),
  unit_price_cents: z.number({ message: "Unit price must be a number" })
    .int("Unit price must be whole cents")
    .min(0, "Unit price must be zero or more"),
  category: z.string().trim().max(100, "Category must be 100 characters or fewer"),
});

export const requisitionFormSchema = z.object({
  currency: currencySchema,
  items: z.array(itemSchema),
});

export type RequisitionFormValues = z.infer<typeof requisitionFormSchema>;
export type ItemFormValues = z.infer<typeof itemSchema>;

// Step slices: the wizard triggers these on "Next" so a step only advances
// when its own fields are valid.
export const detailsStepSchema = z.object({ currency: currencySchema });
export const itemsStepSchema = z.object({ items: z.array(itemSchema) });

export function itemsAreValid(items: unknown): boolean {
  return itemsStepSchema.safeParse({ items }).success;
}
