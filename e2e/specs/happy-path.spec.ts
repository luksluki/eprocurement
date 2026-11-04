// Issue #59 — Playwright happy-path e2e: the demo journey end to end against
// the compose stack (gateway :8080) and the production web build
// (`next start` :3000), per ADR-0002's e2e stage.
//
// The story runs as ONE sequential test so each step feeds the next:
//
//   1. login as tenant A's admin via /login, then tenant switch through the
//      TenantSwitcher with the vendor list proving the data changed
//      (tenant isolation, #50's acceptance shape);
//   2. create + submit a requisition through the 4-step wizard as tenant A's
//      admin (requester);
//   3. logout, login as a SECOND user of the SAME tenant (self-approval is
//      forbidden for everyone, admins included — docs/rbac.md #30), and
//      approve — with the requester's own view asserted action-less first;
//   4. convert to a PO through the gateway API directly (maintainer decision
//      on issue #59: there is no convert UI; the gap is a separate issue);
//   5. drive the PO lifecycle draft → issued → acknowledged in the UI, then
//      record a full goods receipt (GRN requires acknowledged+, #37);
//   6. create the invoice for exactly the received quantities, upload a
//      VALID PNG attachment until it reaches ready (the hard Match gate,
//      #41), run the match, and assert the matched verdict (#40).
//
// Fixtures (users, vendors, approval rules) are installed by the
// globalSetup — this spec owns its data (maintainer decision #2).
import { resolve } from "node:path";

import { expect, test, type Page } from "@playwright/test";

import {
  E2E_ADMIN_PASSWORD,
  REQUISITION_ITEM,
  TENANT_A_ADMIN_EMAIL,
  TENANT_A_APPROVER_EMAIL,
  TENANT_A_ID,
  TENANT_A_VENDOR,
  TENANT_B_ID,
  TENANT_B_VENDOR,
  WEB_URL,
} from "../support/fixtures";
import { convertRequisitionToPO, findVendorByCode, gatewayLogin } from "../support/gateway";

// Playwright transpiles the specs to CommonJS, so __dirname resolves —
// import.meta.url would not (no ESM shim in the test loader).
const attachmentPng = resolve(__dirname, "../fixtures/invoice-attachment.png");

// Pin the EN dictionary via the locale cookie so the assertions on UI copy
// (button names, statuses, verdicts) are deterministic regardless of the
// app's DEFAULT_LOCALE.
async function pinEnglish(page: Page): Promise<void> {
  await page.context().addCookies([{ name: "eproc_locale", value: "en", url: new URL(WEB_URL).origin }]);
}

async function loginViaUI(page: Page, email: string): Promise<void> {
  await page.goto("/login");
  await page.locator("#email").fill(email);
  await page.locator("#password").fill(E2E_ADMIN_PASSWORD);
  await page.getByRole("button", { name: "Sign in" }).click();
}

test.describe("happy path", () => {
  test("login → tenant switch → PR → approve → PO → GRN → invoice match", async ({ page }) => {
    await pinEnglish(page);

    // ---- 1. Login as tenant A's admin (the requester) ------------------
    await loginViaUI(page, TENANT_A_ADMIN_EMAIL);
    await page.waitForURL(/\/app\//);
    // The shell header shows the session's tenant id (the dashboard body
    // repeats it — scope to the banner).
    await expect(page.getByRole("banner").getByText(TENANT_A_ID)).toBeVisible();

    // ---- 2. Tenant switch with visible data change (isolation) ---------
    await page.goto("/app/vendors");
    await expect(page.getByText(TENANT_A_VENDOR.name)).toBeVisible();

    await page.locator("#tenant-switcher").selectOption("glow");
    await expect(page.getByText(TENANT_B_VENDOR.name)).toBeVisible();
    await expect(page.getByText(TENANT_A_VENDOR.name)).toHaveCount(0);
    await expect(page.getByRole("banner").getByText(TENANT_B_ID)).toBeVisible();

    // Switch back to tenant A — the requester continues the flow.
    await page.locator("#tenant-switcher").selectOption("acme");
    await expect(page.getByText(TENANT_A_VENDOR.name)).toBeVisible();
    await expect(page.getByText(TENANT_B_VENDOR.name)).toHaveCount(0);

    // ---- 3. Requisition through the 4-step wizard -----------------------
    await page.goto("/app/requisitions/new");

    // Choosing the currency creates the draft (?draft=<id>); the URL change
    // also fires the wizard's restore fetch, whose form.reset would wipe
    // anything typed before it lands. Capture that response up front and
    // wait it out before filling the basket.
    const restoreDone = page.waitForResponse(
      (res) => res.request().method() === "GET" && res.url().includes("/api/requisitions/"),
      { timeout: 30_000 },
    );

    await page.locator('[data-testid="input-currency"]').selectOption("IDR");
    await page.locator('[data-testid="next-from-details"]').click();
    await page.waitForURL(/[?&]draft=/);
    const draftParam = new URL(page.url()).searchParams.get("draft");
    expect(draftParam).toBeTruthy();
    const requisitionId: string = draftParam as string;
    await restoreDone;

    // Items: exactly one line — qty and price flow into the PO, the GRN,
    // and the invoice, so the match verdict is deterministic. The basket
    // autosaves through a DEBOUNCED PUT (800 ms after the last edit; the
    // "Draft saved." badge alone is stale until it fires) — capture the PUT
    // and wait for it before submitting, or the server would reject an
    // empty basket with requisition_items_empty.
    const itemsSaved = page.waitForResponse(
      (res) => res.request().method() === "PUT" && res.url().includes("/api/requisitions/"),
      { timeout: 30_000 },
    );
    await page.locator('[data-testid="item-description-0"]').fill(REQUISITION_ITEM.description);
    await page.locator('[data-testid="item-qty-0"]').fill(REQUISITION_ITEM.qty);
    await page.locator('[data-testid="item-unit-price-0"]').fill(REQUISITION_ITEM.unitPriceCents);
    await page.locator('[data-testid="item-category-0"]').fill(REQUISITION_ITEM.category);
    await page.locator('[data-testid="next-from-items"]').click();

    // Attachments: none needed on the requisition (the invoice attachment
    // is the one the Match gate requires).
    await page.locator('[data-testid="next-from-attachments"]').click();

    // Review: the total mirrors the single line (2 × 7,500,000 cents).
    await expect(page.locator('[data-testid="review-total"]')).toContainText("15,000,000");
    await itemsSaved;
    await expect(page.locator('[data-testid="autosave-status"]')).toContainText("Draft saved.");
    await page.locator('[data-testid="submit-requisition"]').click();
    await expect(page.locator('[data-testid="submitted"]')).toBeVisible();

    // The requester cannot see any approval actions on their own
    // requisition — self-approval is forbidden for everyone, admins
    // included (apps/web/src/lib/approval-eligibility.ts).
    await page.goto(`/app/requisitions/${requisitionId}`);
    await expect(page.locator('[data-testid="detail-status"]')).toHaveText("submitted");
    await expect(page.locator('[data-testid="approval-actions"]')).toHaveCount(0);

    // ---- 4. Second user of the SAME tenant approves ---------------------
    await page.getByRole("button", { name: "Sign out" }).click();
    await page.waitForURL(/\/login/);

    await loginViaUI(page, TENANT_A_APPROVER_EMAIL);
    await page.waitForURL(/\/app\//);

    await page.goto(`/app/requisitions/${requisitionId}`);
    await expect(page.locator('[data-testid="approval-actions"]')).toBeVisible();
    await page.locator('[data-testid="approval-reason"]').fill("e2e happy-path approval");
    await page.locator('[data-testid="approval-approve"]').click();
    await expect(page.locator('[data-testid="detail-status"]')).toHaveText("approved");

    // ---- 5. Convert to a PO via the gateway (no convert UI — issue #59
    // maintainer decision; the UI gap is tracked separately) --------------
    const token = await gatewayLogin(TENANT_A_APPROVER_EMAIL, E2E_ADMIN_PASSWORD);
    const vendor = await findVendorByCode(token, TENANT_A_VENDOR.code);
    expect(vendor).not.toBeNull();
    const po = await convertRequisitionToPO(token, requisitionId, (vendor as NonNullable<typeof vendor>).id);

    // ---- 6. PO lifecycle: draft → issued → acknowledged → full GRN ------
    await page.goto(`/app/purchase-orders/${po.id}`);
    await expect(page.locator('[data-testid="detail-number"]')).toHaveText(po.number);
    await expect(page.locator('[data-testid="detail-status"]')).toHaveText("draft");

    await page.locator('[data-testid="po-issue"]').click();
    await expect(page.locator('[data-testid="detail-status"]')).toHaveText("issued");

    // GRN needs acknowledged or partially_received (#37) — acknowledge first.
    await page.locator('[data-testid="po-acknowledge"]').click();
    await expect(page.locator('[data-testid="detail-status"]')).toHaveText("acknowledged");

    const receiptForm = page.locator('[data-testid="goods-receipt-form"]');
    await expect(receiptForm).toBeVisible();
    const qtyInputs = receiptForm.locator('input[data-testid^="grn-qty-"]');
    const lineCount = await qtyInputs.count();
    expect(lineCount).toBeGreaterThan(0);
    for (let i = 0; i < lineCount; i++) {
      // The input's max IS the line's remaining quantity — receive it all.
      const remaining = (await qtyInputs.nth(i).getAttribute("max")) ?? REQUISITION_ITEM.qty;
      await qtyInputs.nth(i).fill(remaining);
    }
    await page.locator('[data-testid="grn-submit"]').click();
    // A full receipt flips the PO to received — which unmounts the receipt
    // form itself (only acknowledged/partially_received are receivable, so
    // the grn-confirmation banner does not survive the refresh). The
    // durable signals are the status and the received line in the table.
    await expect(page.locator('[data-testid="detail-status"]')).toHaveText("received");
    await expect(page.locator('[data-testid="detail-items"] tbody tr').first()).toContainText(
      REQUISITION_ITEM.description,
    );

    // ---- 7. Invoice for exactly the received quantities -----------------
    await page.locator('[data-testid="po-create-invoice-link"]').click();
    await page.waitForURL(/\/app\/invoices\/new\?po=/);

    const invoiceNumber = `E2E-INV-${Date.now()}`;
    await page.locator('[data-testid="invoice-number"]').fill(invoiceNumber);
    const invoiceQtyInputs = page.locator('input[data-testid^="invoice-qty-"]');
    const invoiceLineCount = await invoiceQtyInputs.count();
    for (let i = 0; i < invoiceLineCount; i++) {
      // Bill the full ordered quantity at the prefilled PO price.
      await invoiceQtyInputs.nth(i).fill(REQUISITION_ITEM.qty);
    }
    await page.locator('[data-testid="invoice-submit"]').click();
    await page.waitForURL(/\/app\/invoices\/[0-9a-f-]{36}/);
    await expect(page.locator('[data-testid="detail-number"]')).toHaveText(invoiceNumber);
    await expect(page.locator('[data-testid="detail-status"]')).toHaveText("pending");

    // ---- 8. Attachment (a VALID PNG — the mime allowlist rejects anything
    // else and the Match button stays gated until an attachment is ready) --
    await page.locator('[data-testid="invoice-upload-input"]').setInputFiles(attachmentPng);
    await expect(page.locator('[data-testid="invoice-attachments-table"]')).toContainText("ready");
    await expect(page.locator('[data-testid="invoice-match-gate"]')).toHaveCount(0);

    // ---- 9. Run the three-way match -------------------------------------
    await page.locator('[data-testid="invoice-match"]').click();
    await expect(page.locator('[data-testid="match-result-panel"]')).toBeVisible();
    await expect(page.locator('[data-testid="match-verdict"]')).toContainText("Matched");
    await expect(page.locator('[data-testid="detail-status"]')).toHaveText("matched");
  });
});
