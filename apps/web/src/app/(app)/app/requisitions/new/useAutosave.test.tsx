import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { useAutosave, type AutosaveOutcome } from "./useAutosave";
import type { RequisitionItemWire } from "@/lib/gateway";

const item: RequisitionItemWire = { description: "Laptop", qty: 2, unit_price_cents: 7_500_000, category: "it" };

async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

function renderAutosave(overrides: Partial<Parameters<typeof useAutosave>[0]> = {}) {
  const createDraft = vi.fn(async (): AutosaveOutcome => ({ ok: true, id: "pr-1" }));
  const saveItems = vi.fn(async (): AutosaveOutcome => ({ ok: true }));
  const onDraftCreated = vi.fn();
  const hook = renderHook((props: Partial<Parameters<typeof useAutosave>[0]> = {}) =>
    useAutosave({
      currency: "IDR",
      detailsValid: true,
      itemsValid: true,
      draftId: null,
      items: [],
      createDraft,
      saveItems,
      onDraftCreated,
      ...overrides,
      ...props,
    }),
  );
  return { hook, createDraft, saveItems, onDraftCreated };
}

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
});

afterEach(() => {
  vi.useRealTimers();
});

describe("useAutosave (#33)", () => {
  it("does not create the draft until the details step is valid", async () => {
    const { hook, createDraft } = renderAutosave({ detailsValid: false });
    await advance(2000);
    expect(createDraft).not.toHaveBeenCalled();
    expect(hook.result.current.status).toBe("idle");
  });

  it("creates the draft exactly once when the currency is chosen", async () => {
    const { hook, createDraft, onDraftCreated } = renderAutosave();
    await advance(50);
    await waitFor(() => expect(onDraftCreated).toHaveBeenCalledWith("pr-1"));
    expect(createDraft).toHaveBeenCalledTimes(1);
    expect(hook.result.current.status).toBe("saved");

    // Settling further must not create a second draft.
    await advance(100);
    expect(createDraft).toHaveBeenCalledTimes(1);
  });

  it("adopts the draft id when deps change mid-create — no second POST, autosave continues as PATCH", async () => {
    let resolveCreate: (outcome: AutosaveOutcome) => void = () => {};
    const createDraft = vi.fn(
      () =>
        new Promise<AutosaveOutcome>((resolve) => {
          resolveCreate = resolve;
        }),
    );
    const saveItems = vi.fn(async (): AutosaveOutcome => ({ ok: true }));
    const onDraftCreated = vi.fn();
    const hook = renderHook(
      (props: { draftId: string | null; items: RequisitionItemWire[] }) =>
        useAutosave({
          currency: "IDR",
          detailsValid: true,
          itemsValid: true,
          draftId: props.draftId,
          items: props.items,
          createDraft,
          saveItems,
          onDraftCreated,
          debounceMs: 300,
        }),
      { initialProps: { draftId: null, items: [] as RequisitionItemWire[] } },
    );

    await advance(50);
    expect(createDraft).toHaveBeenCalledTimes(1); // POST in flight

    // The user edits the basket while the POST is still out: the effect run
    // is cancelled, but the request (and the draft it creates) lives on.
    await act(async () => {
      hook.rerender({ draftId: null, items: [item] });
    });
    expect(createDraft).toHaveBeenCalledTimes(1);

    // Resolve after cancellation: the id must be adopted, not dropped.
    await act(async () => {
      resolveCreate({ ok: true, id: "pr-1" });
    });
    expect(onDraftCreated).toHaveBeenCalledTimes(1);
    expect(onDraftCreated).toHaveBeenCalledWith("pr-1");

    // The wizard applies the id via onDraftCreated; the changed basket then
    // autosaves as a PATCH against that draft — never a second POST.
    await act(async () => {
      hook.rerender({ draftId: "pr-1", items: [item] });
    });
    await advance(100);
    expect(createDraft).toHaveBeenCalledTimes(1);
    expect(saveItems).not.toHaveBeenCalled(); // still inside the debounce
    await advance(300);
    expect(saveItems).toHaveBeenCalledTimes(1);
    expect(saveItems).toHaveBeenCalledWith("pr-1", [item]);
    expect(createDraft).toHaveBeenCalledTimes(1);
  });

  it("PUTs the basket after the debounce once a draft exists", async () => {
    const saveItems = vi.fn(async (): AutosaveOutcome => ({ ok: true }));
    const hook = renderHook(
      (props: { items: RequisitionItemWire[] }) =>
        useAutosave({
          currency: "IDR",
          detailsValid: true,
          itemsValid: true,
          draftId: "pr-1",
          items: props.items,
          createDraft: vi.fn(async () => ({ ok: false, status: 502, error: "x", code: "gateway_unavailable" })),
          saveItems,
          onDraftCreated: vi.fn(),
          debounceMs: 300,
        }),
      { initialProps: { items: [] as RequisitionItemWire[] } },
    );

    await act(async () => {
      hook.rerender({ items: [] });
    });
    await advance(50);
    expect(saveItems).not.toHaveBeenCalled(); // baseline: nothing to save

    await act(async () => {
      hook.rerender({ items: [item] });
    });
    await advance(100);
    expect(saveItems).not.toHaveBeenCalled(); // still inside the debounce
    await advance(300);
    expect(saveItems).toHaveBeenCalledTimes(1);
    expect(saveItems).toHaveBeenCalledWith("pr-1", [item]);

    // Same basket again: no duplicate PUT.
    await act(async () => {
      hook.rerender({ items: [item] });
    });
    await advance(600);
    expect(saveItems).toHaveBeenCalledTimes(1);
  });

  it("skips the PUT while the basket is invalid (mid-edit rows)", async () => {
    const saveItems = vi.fn(async (): AutosaveOutcome => ({ ok: true }));
    const hook = renderHook(() =>
      useAutosave({
        currency: "IDR",
        detailsValid: true,
        itemsValid: false,
        draftId: "pr-1",
        items: [{ ...item, qty: Number.NaN }],
        createDraft: vi.fn(),
        saveItems,
        onDraftCreated: vi.fn(),
      }),
    );
    expect(hook.result.current).toBeTruthy();
    await advance(2000);
    expect(saveItems).not.toHaveBeenCalled();
  });

  it("stops permanently on 403 requisition_forbidden — no retry loop", async () => {
    const saveItems = vi.fn(async (): AutosaveOutcome => ({
      ok: false,
      status: 403,
      error: "only the requester may modify",
      code: "requisition_forbidden",
    }));
    const hook = renderHook(
      (props: { items: RequisitionItemWire[] }) =>
        useAutosave({
          currency: "IDR",
          detailsValid: true,
          itemsValid: true,
          draftId: "pr-1",
          items: props.items,
          createDraft: vi.fn(),
          saveItems,
          onDraftCreated: vi.fn(),
        }),
      { initialProps: { items: [] as RequisitionItemWire[] } },
    );
    await act(async () => {
      hook.rerender({ items: [item] });
    });
    await advance(1000);
    expect(saveItems).toHaveBeenCalledTimes(1);
    expect(hook.result.current.stopped).toBe(true);
    expect(hook.result.current.code).toBe("requisition_forbidden");
    expect(hook.result.current.message).toContain("another user");

    // Later changes must not fire another save.
    await act(async () => {
      hook.rerender({ items: [item, { ...item, description: "Mouse" }] });
    });
    await advance(2000);
    expect(saveItems).toHaveBeenCalledTimes(1);
  });

  it("stops permanently on 409 requisition_illegal_transition (non-draft)", async () => {
    const saveItems = vi.fn(async (): AutosaveOutcome => ({
      ok: false,
      status: 409,
      error: "no longer a draft",
      code: "requisition_illegal_transition",
    }));
    const hook = renderHook(() =>
      useAutosave({
        currency: "IDR",
        detailsValid: true,
        itemsValid: true,
        draftId: "pr-1",
        items: [item],
        createDraft: vi.fn(),
        saveItems,
        onDraftCreated: vi.fn(),
      }),
    );
    await advance(1000);
    expect(hook.result.current.stopped).toBe(true);
    expect(hook.result.current.code).toBe("requisition_illegal_transition");
  });

  it("seed() suppresses the immediate PUT of restored items", async () => {
    const saveItems = vi.fn(async (): AutosaveOutcome => ({ ok: true }));
    const hook = renderHook(() =>
      useAutosave({
        currency: "IDR",
        detailsValid: true,
        itemsValid: true,
        draftId: "pr-1",
        items: [item],
        createDraft: vi.fn(),
        saveItems,
        onDraftCreated: vi.fn(),
      }),
    );
    act(() => hook.result.current.seed([item]));
    await advance(2000);
    expect(saveItems).not.toHaveBeenCalled(); // restored basket is already stored
  });

  it("treats transient failures as retryable (not stopped)", async () => {
    const saveItems = vi.fn(async (): AutosaveOutcome => ({
      ok: false,
      status: 502,
      error: "gateway unreachable",
      code: "gateway_unavailable",
    }));
    const hook = renderHook(() =>
      useAutosave({
        currency: "IDR",
        detailsValid: true,
        itemsValid: true,
        draftId: "pr-1",
        items: [item],
        createDraft: vi.fn(),
        saveItems,
        onDraftCreated: vi.fn(),
      }),
    );
    await advance(1000);
    expect(hook.result.current.status).toBe("error");
    expect(hook.result.current.stopped).toBe(false);
  });
});
