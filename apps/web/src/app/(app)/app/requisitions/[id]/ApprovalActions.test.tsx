// ApprovalActions tests (issue #34): render/hide follows the eligibility
// step prop (null hides — AC "actions hidden when not permitted"), a reject
// without a reason never leaves the browser (edge contract mirrored), a
// success POSTs with a per-attempt Idempotency-Key and refreshes the server
// component, and 4xx machine codes render as mapped messages.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import ApprovalActions from "./ApprovalActions";

const mocks = vi.hoisted(() => ({
  refresh: vi.fn(),
  assign: vi.fn(),
}));

vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: mocks.refresh }),
  usePathname: () => "/app/requisitions/pr-1",
}));

const copy = {
  approve: "Approve",
  reject: "Reject",
  reasonLabel: "Reason (required to reject)",
  reasonPlaceholder: "Reason for the decision…",
  reasonRequired: "A reject requires a reason.",
  reasonTooLong: "Reason must be at most 1000 characters.",
  submitting: "Deciding…",
  genericError: "Failed to decide — try again.",
  sessionExpired: "Session expired — redirecting to sign-in…",
  errorMessages: {
    approval_step_not_current: "This step is no longer the current pending step.",
    approval_role_forbidden: "This step requires a role you do not hold.",
  },
};

const step = { id: "step-1", step_no: 1, role_required: "approver" };

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

const originalLocation = window.location;

beforeEach(() => {
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { ...originalLocation, assign: mocks.assign },
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  mocks.refresh.mockClear();
  mocks.assign.mockClear();
  Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
});

function renderActions(stepProp: typeof step | null = step) {
  return render(<ApprovalActions step={stepProp} copy={copy} />);
}

describe("ApprovalActions", () => {
  it("renders the decision controls when a decidable step is given", () => {
    renderActions();
    expect(screen.getByTestId("approval-actions")).toBeTruthy();
    expect(screen.getByTestId("approval-approve").textContent).toBe("Approve");
    expect(screen.getByTestId("approval-reject").textContent).toBe("Reject");
    expect(screen.queryByTestId("approval-error")).toBeNull();
  });

  it("renders nothing when the step is null (not permitted)", () => {
    const { container } = renderActions(null);
    expect(container.firstChild).toBeNull();
    expect(screen.queryByTestId("approval-approve")).toBeNull();
  });

  it("rejects without a reason locally: error shown, no network call", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    renderActions();
    fireEvent.click(screen.getByTestId("approval-reject"));

    const error = await screen.findByTestId("approval-error");
    expect(error.textContent).toBe(copy.reasonRequired);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("approves: POSTs with a per-attempt Idempotency-Key and refreshes on success", async () => {
    const fetchMock = vi.fn(async () => jsonResponse({ id: "pr-1", status: "approved" }));
    vi.stubGlobal("fetch", fetchMock);

    renderActions();
    fireEvent.click(screen.getByTestId("approval-approve"));

    await waitFor(() => expect(mocks.refresh).toHaveBeenCalledTimes(1));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/approvals/step-1/decide");
    expect(init.method).toBe("POST");
    expect(JSON.parse(String(init.body))).toEqual({ decision: "approve", reason: "" });
    // The key is a fresh UUID per attempt (crypto.randomUUID).
    const key = (init.headers as Record<string, string>)["idempotency-key"];
    expect(key).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
    // No error surfaced on success.
    expect(screen.queryByTestId("approval-error")).toBeNull();
  });

  it("rejects with a reason: forwards the trimmed reason in the body", async () => {
    const fetchMock = vi.fn(async () => jsonResponse({ id: "pr-1", status: "rejected" }));
    vi.stubGlobal("fetch", fetchMock);

    renderActions();
    fireEvent.change(screen.getByTestId("approval-reason"), { target: { value: "  budget exceeded  " } });
    fireEvent.click(screen.getByTestId("approval-reject"));

    await waitFor(() => expect(mocks.refresh).toHaveBeenCalledTimes(1));
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(String(init.body))).toEqual({ decision: "reject", reason: "budget exceeded" });
  });

  it("shows the mapped message for a 4xx machine code (lost race)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          jsonResponse(
            { error: "this step is not the current pending step", code: "approval_step_not_current" },
            409,
          ),
      ),
    );

    renderActions();
    fireEvent.click(screen.getByTestId("approval-approve"));

    const error = await screen.findByTestId("approval-error");
    expect(error.textContent).toBe(copy.errorMessages.approval_step_not_current);
    expect(mocks.refresh).not.toHaveBeenCalled();
  });

  it("falls back to the generic message for an unmapped code", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse({ error: "boom", code: "something_new" }, 400)),
    );

    renderActions();
    fireEvent.click(screen.getByTestId("approval-approve"));

    const error = await screen.findByTestId("approval-error");
    expect(error.textContent).toBe(copy.genericError);
  });

  it("runs the refresh dance on 401 with the session-expired notice", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "Session expired." }, 401)));

    renderActions();
    fireEvent.click(screen.getByTestId("approval-approve"));

    await waitFor(() =>
      expect(mocks.assign).toHaveBeenCalledWith("/api/auth/refresh?next=%2Fapp%2Frequisitions%2Fpr-1"),
    );
    expect((await screen.findByTestId("approval-error")).textContent).toBe(copy.sessionExpired);
  });
});
