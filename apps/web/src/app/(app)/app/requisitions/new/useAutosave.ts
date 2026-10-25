"use client";

// Draft autosave (#33). The hook owns exactly two jobs and nothing else:
// create the server-side draft once the details step is valid (the domain
// rejects an empty currency, so creation must wait for a valid choice), then
// debounce-PUT the basket whenever it changes and is valid. A 403 (foreign
// draft) or 409 (no longer draft) is fatal: autosave stops permanently and
// surfaces the machine code so the wizard can render an error state without
// a retry loop. Transient failures (network/5xx) only mark the last attempt
// failed — the next change retries.
//
// The effect keys on the basket's serialized form, not the array identity:
// callers rebuild the items array every render (useWatch → toWire), and an
// identity-keyed effect would cancel the in-flight request on every
// unrelated state change (e.g. this hook's own "saving" setState).
import { useCallback, useEffect, useRef, useState } from "react";
import type { RequisitionItemWire } from "@/lib/gateway";

export type AutosaveOutcome =
  | { ok: true; id?: string }
  | { ok: false; status: number; error: string; code: string };

export type AutosaveState = {
  status: "idle" | "saving" | "saved" | "error";
  message: string | null;
  code: string | null;
  stopped: boolean;
};

const FATAL_STATUSES = new Set([403, 409]);

const fatalMessages: Record<string, string> = {
  requisition_forbidden:
    "This draft belongs to another user — autosave stopped. Reload without the ?draft link to start your own.",
  requisition_illegal_transition:
    "This requisition is no longer a draft (it may have been submitted elsewhere) — autosave stopped.",
};

function fatalMessage(code: string, fallback: string): string {
  return fatalMessages[code] ?? fallback;
}

export function useAutosave(opts: {
  currency: string;
  detailsValid: boolean;
  itemsValid: boolean;
  draftId: string | null;
  items: RequisitionItemWire[];
  createDraft: (currency: string) => Promise<AutosaveOutcome>;
  saveItems: (id: string, items: RequisitionItemWire[]) => Promise<AutosaveOutcome>;
  onDraftCreated: (id: string) => void;
  debounceMs?: number;
}): AutosaveState & { seed: (seedItems: RequisitionItemWire[]) => void } {
  const { currency, detailsValid, itemsValid, draftId, createDraft, saveItems, onDraftCreated } = opts;
  const debounceMs = opts.debounceMs ?? 800;
  const serialized = JSON.stringify(opts.items);

  const [state, setState] = useState<AutosaveState>({ status: "idle", message: null, code: null, stopped: false });
  const lastSavedRef = useRef<string | null>(null); // serialized basket of the last successful PUT
  const creatingRef = useRef(false);
  const inFlightRef = useRef(false);
  const stoppedRef = useRef(false);

  // seed marks a basket as already stored — the restore path calls it so the
  // freshly loaded items do not trigger an immediate PUT.
  const seed = useCallback((seedItems: RequisitionItemWire[]) => {
    lastSavedRef.current = JSON.stringify(seedItems);
  }, []);

  // Callbacks ride in a ref: their identities churn with the caller's
  // renders (inline objects, fresh router wrappers), and the effect below
  // must key on values only — identity churn would cancel in-flight saves
  // and reset the debounce on every unrelated re-render.
  const callbacksRef = useRef({ createDraft, saveItems, onDraftCreated });
  callbacksRef.current = { createDraft, saveItems, onDraftCreated };

  useEffect(() => {
    if (stoppedRef.current) {
      return;
    }
    if (draftId === null) {
      // Create as soon as the details step is valid; exactly once. The
      // debounce-free path is safe: this effect only re-runs when its value
      // deps change, and none of them move while the create is in flight.
      if (!detailsValid || creatingRef.current) {
        return;
      }
      creatingRef.current = true;
      setState((s) => ({ ...s, status: "saving" }));
      let cancelled = false;
      void (async () => {
        const outcome = await callbacksRef.current.createDraft(currency);
        creatingRef.current = false;
        if (cancelled) {
          return;
        }
        if (outcome.ok) {
          lastSavedRef.current = JSON.stringify([]);
          setState({ status: "saved", message: null, code: null, stopped: false });
          if (outcome.id) {
            callbacksRef.current.onDraftCreated(outcome.id);
          }
          return;
        }
        if (FATAL_STATUSES.has(outcome.status)) {
          stoppedRef.current = true;
          setState({
            status: "error",
            message: fatalMessage(outcome.code, outcome.error),
            code: outcome.code,
            stopped: true,
          });
          return;
        }
        setState({ status: "error", message: outcome.error, code: outcome.code, stopped: false });
      })();
      return () => {
        cancelled = true;
      };
    }

    // Basket autosave: PUT when the serialized basket differs from the last
    // saved one and is valid. Invalid rows (mid-edit) wait for validity.
    if (serialized === lastSavedRef.current || !itemsValid || inFlightRef.current) {
      return;
    }
    const timer = setTimeout(() => {
      // Re-check under the timer: seed() may have marked this basket as
      // already stored between the schedule and the fire.
      if (serialized === lastSavedRef.current || stoppedRef.current) {
        return;
      }
      inFlightRef.current = true;
      setState((s) => ({ ...s, status: "saving" }));
      const snapshot = serialized;
      void (async () => {
        const outcome = await callbacksRef.current.saveItems(draftId, JSON.parse(snapshot) as RequisitionItemWire[]);
        inFlightRef.current = false;
        if (stoppedRef.current) {
          return;
        }
        if (outcome.ok) {
          lastSavedRef.current = snapshot;
          setState({ status: "saved", message: null, code: null, stopped: false });
        } else if (FATAL_STATUSES.has(outcome.status)) {
          stoppedRef.current = true;
          setState({
            status: "error",
            message: fatalMessage(outcome.code, outcome.error),
            code: outcome.code,
            stopped: true,
          });
        } else {
          setState({ status: "error", message: outcome.error, code: outcome.code, stopped: false });
        }
        // A change landed while the request was out: the next effect run
        // sees a stale lastSavedRef and schedules a follow-up save.
      })();
    }, debounceMs);
    return () => clearTimeout(timer);
  }, [currency, detailsValid, draftId, itemsValid, serialized, debounceMs]);

  return { ...state, seed };
}
