// po-eligibility tests (issue #38, AC "actions hidden when not eligible"):
// the table of hidden/shown cases mirrors the server's gates — po:write and
// grn:post are buyer/admin only (docs/rbac.md), issue only from draft,
// acknowledge only from issued, receipts only on acknowledged/partially_
// received (the #36/#37 domain edges).
import { describe, expect, it } from "vitest";
import { canAcknowledgePO, canIssuePO, canPostGoodsReceipt, holdsGRNPost, holdsPOWrite } from "./po-eligibility";

describe("holdsPOWrite / holdsGRNPost", () => {
  it("buyer and admin hold them; every other role does not", () => {
    for (const holds of [holdsPOWrite, holdsGRNPost]) {
      expect(holds(["buyer"])).toBe(true);
      expect(holds(["admin"])).toBe(true);
      expect(holds(["requester"])).toBe(false);
      expect(holds(["approver"])).toBe(false);
      expect(holds(["finance"])).toBe(false);
      expect(holds(["viewer"])).toBe(false);
      expect(holds([])).toBe(false);
      // A multi-role user: one granting role is enough.
      expect(holds(["requester", "buyer"])).toBe(true);
    }
  });
});

describe("canIssuePO (draft → issued)", () => {
  it("is shown only for a buyer/admin on a draft PO", () => {
    expect(canIssuePO(["buyer"], "draft")).toBe(true);
    expect(canIssuePO(["admin"], "draft")).toBe(true);
  });

  it("is hidden for a non-holder regardless of status", () => {
    for (const status of ["draft", "issued", "acknowledged", "partially_received", "received", "closed", "cancelled"]) {
      expect(canIssuePO(["requester"], status)).toBe(false);
      expect(canIssuePO(["viewer"], status)).toBe(false);
    }
  });

  it("is hidden for a buyer on any non-draft status", () => {
    for (const status of ["issued", "acknowledged", "partially_received", "received", "closed", "cancelled"]) {
      expect(canIssuePO(["buyer"], status)).toBe(false);
    }
  });
});

describe("canAcknowledgePO (issued → acknowledged)", () => {
  it("is shown only for a buyer/admin on an issued PO", () => {
    expect(canAcknowledgePO(["buyer"], "issued")).toBe(true);
    expect(canAcknowledgePO(["admin"], "issued")).toBe(true);
    expect(canAcknowledgePO(["buyer"], "draft")).toBe(false);
    expect(canAcknowledgePO(["buyer"], "acknowledged")).toBe(false);
    expect(canAcknowledgePO(["finance"], "issued")).toBe(false);
  });
});

describe("canPostGoodsReceipt (acknowledged/partially_received only)", () => {
  it("is shown for a buyer/admin on the two receiving statuses", () => {
    expect(canPostGoodsReceipt(["buyer"], "acknowledged")).toBe(true);
    expect(canPostGoodsReceipt(["admin"], "partially_received")).toBe(true);
  });

  it("is hidden on every other status — further partials continue, terminals and pre-ack do not start", () => {
    for (const status of ["draft", "issued", "received", "closed", "cancelled"]) {
      expect(canPostGoodsReceipt(["buyer"], status)).toBe(false);
    }
  });

  it("is hidden for a non-holder regardless of status", () => {
    expect(canPostGoodsReceipt(["requester"], "acknowledged")).toBe(false);
    expect(canPostGoodsReceipt(["finance"], "partially_received")).toBe(false);
  });
});
