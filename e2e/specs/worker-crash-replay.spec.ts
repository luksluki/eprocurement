// Issue #60 — Playwright resilience e2e: worker crash and replay (PRD §22
// step 6, FR-12, §14.8, the "one resilience scenario" of §23's MVP AC). The
// happy-path spec (#59) proves the flow works; THIS spec proves the async
// backbone survives the worker dying mid-flow:
//
//   1. stop the worker gracefully and pile submit/approve events in the
//      transactional outbox (§14.8 "worker down": nothing lost);
//   2. start it again — the backlog flushes into RabbitMQ while the
//      projection store is stalled (a brief mongod fsyncLock), which pins
//      the consumer mid-handler with unacknowledged deliveries on BOTH
//      projection queues — a stable, OBSERVED state;
//   3. SIGKILL the worker (docker compose kill -s SIGKILL) at that observed
//      in-flight state and prove the backlog is durable on the broker:
//      depth > 0 with consumers == 0 on documents.history AND
//      notifications, held across the story's continuation (§14.8 "queue
//      retains messages");
//   4. finish the procure-to-pay story (convert → issue → acknowledge →
//      full GRN) while the worker is DEAD — the sync Postgres truth moves,
//      the events pile up unpublished in the outbox;
//   5. restart the worker and prove the replay loses nothing and duplicates
//      nothing: every event the run created appears EXACTLY ONCE per entity
//      in the event history; Mongo's projections have count == distinct
//      event_id; exactly one notification per notification-routed event;
//      exactly one grn.posted and one audit post row; the PO ends
//      `received` in API and UI.
//
// Crash-gate mechanics (why the fsyncLock, see the dev note for the
// measurements): the worker is ONE binary — its consumer settles each
// delivery within milliseconds of the publisher publishing it, so a healthy
// queue never accumulates depth and the in-flight window is ~100 ms per 2 s
// publisher tick; the management API aggregates queue stats coarsely, and a
// bare SIGKILL fired at an observed burst still races the docker CLI's
// latency against the consumer's drain. Stalling the projection store
// instead (mongod's backup write lock — §14.8's "Mongo down" row, minus the
// crash) holds the consumer mid-Apply with unacknowledged deliveries and a
// growing ready depth: a state the gate can read (rabbitmqctl, live) and
// the kill cannot miss. The lock is released immediately after the kill.
//
// The story entities live on the spec's own tenant (fixtures.ts tenant C),
// so the exactly-once assertions are exact over this run's entities even
// when the happy-path spec or a retry ran before.
import { expect, test, type Page } from "@playwright/test";

import {
  E2E_ADMIN_PASSWORD,
  TENANT_C_ADMIN_EMAIL,
  TENANT_C_APPROVER_EMAIL,
  TENANT_C_ID,
  TENANT_C_VENDOR,
  WEB_URL,
} from "../support/fixtures";
import {
  acknowledgePurchaseOrder,
  convertRequisitionToPO,
  createRequisition,
  decideApproval,
  findVendorByCode,
  gatewayLogin,
  getPurchaseOrder,
  issuePurchaseOrder,
  listEvents,
  listEntityEvents,
  postGoodsReceipt,
  submitRequisition,
  type GatewayEvent,
} from "../support/gateway";
import {
  DLQ_QUEUE,
  DOCUMENTS_HISTORY_QUEUE,
  NOTIFICATIONS_QUEUE,
  queueStats,
  waitForAllQueues,
  waitForQueue,
} from "../support/rabbitmq";
import {
  brokerQueueDepths,
  ensureWorkerRunning,
  killWorker,
  mongoEval,
  mongoFsyncLock,
  mongoFsyncUnlock,
  startWorker,
  stopWorkerGracefully,
} from "../support/stack";

// The backlog created while the worker is gracefully stopped: 120 approved
// PRs = 360 outbox rows. Enough that the post-start ticks keep flushing for
// many seconds — some rows are consumed before the stall, the stall pins
// the rest in-flight/ready, and the remainder replays from the outbox after
// the final restart. Created through the real gateway API in parallel
// lanes (the API is stateless per requisition).
const BACKLOG_APPROVED_PRS = 120;
const BACKLOG_LANES = 6;

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

// One approved requisition through the gateway (three outbox rows:
// pr.submitted + pr.step_approved + pr.approved).
async function createApprovedPR(
  adminToken: string,
  approverToken: string,
  description: string,
): Promise<string> {
  const created = await createRequisition(adminToken, {
    description,
    qty: 2,
    unitPriceCents: 7500000,
    category: "it",
  });
  const submitted = await submitRequisition(adminToken, created.id);
  const step = submitted.approval_steps.find((s) => s.status === "pending");
  if (!step) {
    throw new Error(`requisition ${created.id} has no pending approval step`);
  }
  const decided = await decideApproval(approverToken, step.id, "approve", "e2e resilience approval");
  expect(decided.status, "single-step chain: one approve terminal-approves").toBe("approved");
  return created.id;
}

// Expected event types per entity (the §14.6 catalog writes procurement
// emits for this story shape): an approved requisition carries the submit
// plus the step+terminal decision pair; the PO aggregate carries its whole
// lifecycle including the receipt.
const EXPECTED_PR_TYPES = ["pr.approved", "pr.step_approved", "pr.submitted"];
const EXPECTED_PO_TYPES = ["grn.posted", "po.acknowledged", "po.created", "po.issued"];

function assertExactlyOnce(entityId: string, events: GatewayEvent[], expectedTypes: string[]): void {
  const ids = events.map((e) => e.event_id);
  expect(
    new Set(ids).size,
    `entity ${entityId}: event_ids must be unique (no duplicated projections)`,
  ).toBe(ids.length);
  expect(
    events.map((e) => e.type).sort(),
    `entity ${entityId}: exactly-once event set`,
  ).toEqual(expectedTypes.slice().sort());
}

// multiEquals reports whether two type lists are the same multiset.
function multiEquals(actual: string[], expected: string[]): boolean {
  if (actual.length !== expected.length) {
    return false;
  }
  const a = [...actual].sort();
  const e = [...expected].sort();
  return a.every((v, i) => v === e[i]);
}

test.describe("worker crash and replay (#60)", () => {
  test.afterAll(async () => {
    // Leave the stack healthy whatever happened inside the test (a failed
    // attempt must not poison a CI retry or the maintainer's local stack):
    // release a leftover projection-store stall, then make sure the worker
    // is up and consuming.
    await mongoFsyncUnlock().catch(() => undefined);
    await ensureWorkerRunning().catch(() => undefined);
  });

  test("SIGKILL mid-flow → durable RabbitMQ backlog → replay loses nothing, duplicates nothing", async ({ page }) => {
    test.setTimeout(240_000);

    // ---- 0. Clean slate: worker running, both projection queues empty ----
    const adminToken = await gatewayLogin(TENANT_C_ADMIN_EMAIL, E2E_ADMIN_PASSWORD);
    const approverToken = await gatewayLogin(TENANT_C_APPROVER_EMAIL, E2E_ADMIN_PASSWORD);
    const vendor = await findVendorByCode(adminToken, TENANT_C_VENDOR.code);
    expect(vendor, "tenant C fixture vendor (installed by globalSetup)").not.toBeNull();
    const vendorId = (vendor as NonNullable<typeof vendor>).id;

    await ensureWorkerRunning();
    await waitForAllQueues(
      [DOCUMENTS_HISTORY_QUEUE, NOTIFICATIONS_QUEUE],
      (stats) => stats.every((s) => s.messages === 0),
      30_000,
    );
    // The DLQ baseline: a fresh CI broker starts at 0, but a dev broker may
    // carry history — the post-replay assert is "nothing NEW dead-lettered"
    // (equality), not absolute zero. Read via rabbitmqctl: the management
    // API aggregates coarsely and can serve a stale baseline.
    const dlqBefore = (await brokerQueueDepths()).get(DLQ_QUEUE)?.ready ?? 0;

    // ---- 1. Story part 1: the PR whose PO survives the crash --------------
    const storyPR = await createApprovedPR(adminToken, approverToken, "E2E resilience story PR");
    // Baseline sanity: the async pipeline projects the decision trio.
    const deadline = Date.now() + 30_000;
    for (;;) {
      const seen = await listEntityEvents(approverToken, "requisition", storyPR);
      if (multiEquals(seen.map((e) => e.type), EXPECTED_PR_TYPES)) {
        break;
      }
      if (Date.now() > deadline) {
        throw new Error("baseline: story PR events did not project within 30s");
      }
      await sleep(500);
    }

    // ---- 2. Graceful stop: nothing publishes or consumes from here --------
    await stopWorkerGracefully();

    // ---- 3. Backlog submit/approve events pile up in the outbox -----------
    const backlogIds: string[] = [];
    const perLane = Math.ceil(BACKLOG_APPROVED_PRS / BACKLOG_LANES);
    await Promise.all(
      Array.from({ length: BACKLOG_LANES }, (_, lane) =>
        (async () => {
          for (let i = 0; i < perLane; i++) {
            backlogIds.push(await createApprovedPR(adminToken, approverToken, `E2E resilience backlog PR ${lane}-${i}`));
          }
        })(),
      ),
    );
    expect(backlogIds.length).toBe(BACKLOG_APPROVED_PRS);

    // ---- 4. Start the worker, THEN stall the projection store -------------
    // The worker boots UNLOCKED (verified the hard way: a lock taken before
    // the start blocks the boot's own EnsureIndexes on mongod and consumers
    // never attach). The consumer appearing on documents.history (consumers
    // > 0) proves boot completed — every EnsureIndexes is done — so the
    // stall taken next can only ever pin the consume path. The first ticks
    // flush part of the backlog before the stall lands (consumed normally);
    // the rest keeps the post-stall flow going.
    await startWorker();
    await waitForQueue(DOCUMENTS_HISTORY_QUEUE, (s) => s.consumers > 0, 60_000);
    await mongoFsyncLock();

    // The publisher's 2 s ticks now flush the backlog into RabbitMQ while
    // the consumer's Apply blocks on the stalled store: deliveries hold as
    // unacknowledged (the in-flight proof) and later ticks accumulate ready
    // depth. The gate observes the pinned state on BOTH queues through the
    // broker's own live view (rabbitmqctl — the management API's coarse
    // aggregation cannot see this sub-second state).
    let gate: { ready: number; unacked: number } | null = null;
    let notifGate: { ready: number; unacked: number } | null = null;
    const gateDeadline = Date.now() + 30_000;
    for (;;) {
      const depths = await brokerQueueDepths();
      const hist = depths.get(DOCUMENTS_HISTORY_QUEUE);
      const notif = depths.get(NOTIFICATIONS_QUEUE);
      if (hist && notif && hist.unacked > 0 && notif.unacked > 0) {
        gate = hist;
        notifGate = notif;
        break;
      }
      if (Date.now() > gateDeadline) {
        throw new Error(
          `consumer never pinned with in-flight deliveries within 30s — live depths: hist=${JSON.stringify(hist)} notif=${JSON.stringify(notif)}`,
        );
      }
      await sleep(500);
    }

    // ---- 5. THE CRASH: SIGKILL at the observed in-flight state ------------
    await killWorker();
    // The stall has served its purpose: release immediately so the restart
    // can project normally. (The blocked Apply died with the process; any
    // operation mongod had queued server-side is cancelled with its
    // connection — the redelivered event is deduped by the unique index.)
    await mongoFsyncUnlock();

    // ---- 6. AC1: the backlog is durable on the broker, consumer-less ------
    const afterKill = await waitForAllQueues(
      [DOCUMENTS_HISTORY_QUEUE, NOTIFICATIONS_QUEUE],
      (stats) => stats.every((s) => s.consumers === 0 && s.messages_ready > 0),
      30_000,
    );
    const [histAfterKill, notifAfterKill] = afterKill;
    expect(histAfterKill.consumers, "documents.history has no consumers").toBe(0);
    expect(histAfterKill.messages_ready, "documents.history holds a durable backlog").toBeGreaterThan(0);
    expect(notifAfterKill.consumers, "notifications has no consumers").toBe(0);
    expect(notifAfterKill.messages_ready, "notifications holds a durable backlog (its routing subset)").toBeGreaterThan(0);
    console.log(
      `[e2e #60] crash gate: in-flight at kill hist{ready:${gate.ready},unacked:${gate.unacked}} ` +
        `notif{ready:${notifGate.ready},unacked:${notifGate.unacked}}; durable after kill ` +
        `hist{ready:${histAfterKill.messages_ready}} notif{ready:${notifAfterKill.messages_ready}}`,
    );

    // ---- 7. Story part 2 while the worker is DEAD -------------------------
    // The sync Postgres truth moves normally (the gateway never needs the
    // worker); the four events pile up unpublished in the outbox.
    const po = await convertRequisitionToPO(approverToken, storyPR, vendorId);
    await issuePurchaseOrder(approverToken, po.id);
    await acknowledgePurchaseOrder(approverToken, po.id);
    const acknowledged = await getPurchaseOrder(approverToken, po.id);
    expect(acknowledged.items.length).toBeGreaterThan(0);
    const grn = await postGoodsReceipt(
      approverToken,
      po.id,
      acknowledged.items.map((it) => ({ poItemId: it.id, qty: it.qty_ordered })),
      "e2e resilience full receipt",
    );
    // The receipt is recorded exactly once synchronously: full quantity,
    // PO received (the async side is asserted after the replay).
    expect(grn.purchase_order.status).toBe("received");
    for (const it of grn.purchase_order.items) {
      expect(it.qty_received, "received qty equals ordered qty, not doubled").toBe(it.qty_ordered);
    }

    // §14.8 "queue retains messages": the durable backlog sat untouched the
    // whole time the worker was down.
    const [histWhileDead, notifWhileDead] = await Promise.all([
      queueStats(DOCUMENTS_HISTORY_QUEUE),
      queueStats(NOTIFICATIONS_QUEUE),
    ]);
    expect(histWhileDead.consumers).toBe(0);
    expect(histWhileDead.messages_ready).toBeGreaterThan(0);
    expect(notifWhileDead.consumers).toBe(0);
    expect(notifWhileDead.messages_ready).toBeGreaterThan(0);

    // ---- 8. Restart: the replay drains everything --------------------------
    await startWorker();
    await waitForAllQueues(
      [DOCUMENTS_HISTORY_QUEUE, NOTIFICATIONS_QUEUE],
      (stats) => stats.every((s) => s.messages === 0),
      120_000,
    );
    const dlqAfter = (await brokerQueueDepths()).get(DLQ_QUEUE)?.ready ?? 0;
    expect(
      dlqAfter,
      "nothing new dead-lettered: every replayed event applied (queue depth back to the pre-run baseline)",
    ).toBe(dlqBefore);

    // ---- 9. Wait for the projections to complete, then assert -------------
    const expectedPerEntity = new Map<string, string[]>([
      [storyPR, EXPECTED_PR_TYPES],
      ...backlogIds.map((id) => [id, EXPECTED_PR_TYPES] as [string, string[]]),
      [po.id, EXPECTED_PO_TYPES],
    ]);
    const entityIds = new Set(expectedPerEntity.keys());

    const projected = Date.now() + 90_000;
    let byEntity = new Map<string, GatewayEvent[]>();
    for (;;) {
      const all = await listEvents(approverToken); // whole tenant, keyset pages
      byEntity = new Map<string, GatewayEvent[]>();
      for (const ev of all) {
        if (entityIds.has(ev.entity_id)) {
          const list = byEntity.get(ev.entity_id) ?? [];
          list.push(ev);
          byEntity.set(ev.entity_id, list);
        }
      }
      const complete = [...expectedPerEntity].every(
        ([id, expected]) => multiEquals((byEntity.get(id) ?? []).map((e) => e.type), expected),
      );
      if (complete) {
        break;
      }
      if (Date.now() > projected) {
        throw new Error(`projections did not complete within 90s — entities seen: ${byEntity.size}/${entityIds.size}`);
      }
      await sleep(1_000);
    }

    // AC2 — every event the run created appears exactly once per entity in
    // the event history. (The bulk proof groups the tenant walk; the AC's
    // named per-entity endpoint shape is exercised directly on the story PO.)
    for (const [id, expected] of expectedPerEntity) {
      assertExactlyOnce(id, byEntity.get(id) ?? [], expected);
    }
    const poEventsViaFilter = await listEntityEvents(approverToken, "purchase_order", po.id);
    assertExactlyOnce(po.id, poEventsViaFilter, EXPECTED_PO_TYPES);

    // ---- 10. AC3: no duplicated projections in MongoDB --------------------
    // One mongosh pass over the three collections scoped to this run's
    // entities: count == distinct(event_id) proves the unique-index dedupe
    // held through the at-least-once replay (the killed consumer's
    // unacknowledged deliveries are redelivered; any row the publisher had
    // not yet marked is republished).
    const prCount = 1 + BACKLOG_APPROVED_PRS;
    const expectedEvents = prCount * EXPECTED_PR_TYPES.length + EXPECTED_PO_TYPES.length;
    const expectedNotifications = prCount /* pr.submitted */ + prCount /* pr.approved */ + 2 /* po.issued, po.acknowledged */;
    const expectedAudit = prCount * 2 /* step+terminal per decision */ + 2 /* issue + post on the PO */;

    const idsJson = JSON.stringify([...entityIds]);
    const mongo = (await mongoEval(
      `const tenant='${TENANT_C_ID}';` +
        `const ids=${idsJson};` +
        `const po='${po.id}';` +
        `const q={tenant_id:tenant,entity_id:{$in:ids}};` +
        `const out={counts:{},parity:{},notifTypes:null,auditPO:0,grnPostedPO:0};` +
        `for (const coll of ['events','notifications','audit_log']) {` +
        `out.counts[coll]={count:db[coll].countDocuments(q),distinct:db[coll].distinct('event_id',q).length};}` +
        `for (const ty of ['pr.submitted','pr.approved','po.issued','po.acknowledged']) {` +
        `out.parity[ty]={events:db.events.countDocuments({tenant_id:tenant,type:ty,entity_id:{$in:ids}}),` +
        `notifications:db.notifications.countDocuments({tenant_id:tenant,type:ty,entity_id:{$in:ids}})};}` +
        `out.notifTypes=db.notifications.distinct('type',q).sort();` +
        `out.auditPO=db.audit_log.countDocuments({tenant_id:tenant,entity_id:po});` +
        `out.grnPostedPO=db.events.countDocuments({tenant_id:tenant,entity_id:po,type:'grn.posted'});` +
        `JSON.stringify(out)`,
    )) as {
      counts: Record<string, { count: number; distinct: number }>;
      parity: Record<string, { events: number; notifications: number }>;
      notifTypes: string[];
      auditPO: number;
      grnPostedPO: number;
    };

    for (const coll of ["events", "notifications", "audit_log"]) {
      expect(mongo.counts[coll].count, `${coll}: count equals distinct(event_id) — no duplicates`).toBe(
        mongo.counts[coll].distinct,
      );
    }
    expect(mongo.counts.events.count, "events: exactly the expected number, none lost").toBe(expectedEvents);
    expect(mongo.counts.notifications.count, "notifications: exactly the expected number").toBe(expectedNotifications);
    expect(mongo.counts.audit_log.count, "audit_log: exactly the decision rows").toBe(expectedAudit);
    // Exactly one notification per notification-routed event (PRD §14.5's
    // actor-facing subset): parity per type between the two projections.
    for (const [type, pair] of Object.entries(mongo.parity)) {
      expect(pair.notifications, `one notification per routed ${type} event`).toBe(pair.events);
    }
    expect(mongo.notifTypes, "only routed types reach notifications").toEqual([
      "po.acknowledged",
      "po.issued",
      "pr.approved",
      "pr.submitted",
    ]);

    // ---- 11. AC4: no double receipt ----------------------------------------
    expect(mongo.grnPostedPO, "exactly one grn.posted event for the story PO").toBe(1);
    expect(mongo.auditPO, "exactly two audit rows on the PO (issue + post)").toBe(2);
    const received = await getPurchaseOrder(approverToken, po.id);
    expect(received.status).toBe("received");
    for (const it of received.items) {
      expect(it.qty_received).toBe(it.qty_ordered);
      expect(it.qty_received).toBe(2); // the ordered qty, not doubled
    }
    const grnEvent = (byEntity.get(po.id) ?? []).find((e) => e.type === "grn.posted");
    expect(grnEvent, "the single grn.posted event").toBeDefined();

    // ---- 12. AC5: order status correct after replay, in API and UI --------
    async function loginViaUI(p: Page, email: string): Promise<void> {
      await p.goto("/login");
      await p.locator("#email").fill(email);
      await p.locator("#password").fill(E2E_ADMIN_PASSWORD);
      await p.getByRole("button", { name: "Sign in" }).click();
    }

    await page.context().addCookies([{ name: "eproc_locale", value: "en", url: new URL(WEB_URL).origin }]);
    await loginViaUI(page, TENANT_C_ADMIN_EMAIL);
    await page.waitForURL(/\/app\//);
    await page.goto(`/app/purchase-orders/${po.id}`);
    await expect(page.locator('[data-testid="detail-number"]')).toHaveText(po.number);
    await expect(page.locator('[data-testid="detail-status"]')).toHaveText("received");
    const row = page.locator('[data-testid="detail-items"] tbody tr').first();
    await expect(row).toContainText("E2E resilience story PR");
    const cells = row.locator("td");
    await expect(cells.nth(2), "UI received qty equals ordered qty").toHaveText("2");
    await expect(cells.nth(3), "UI remaining qty is zero — receipt not doubled").toHaveText("0");
    // A received PO offers no receipt form (only acknowledged/partially
    // received are receivable) — the receipt cannot have doubled.
    await expect(page.locator('[data-testid="receipt-form-slot"]')).toHaveCount(0);

    // Census for the report (evidence of the covered catalog).
    console.log(
      `[e2e #60] entities=${prCount + 1} events=${expectedEvents} notifications=${expectedNotifications} ` +
        `audit=${expectedAudit} durable-at-kill=(hist:${histAfterKill.messages_ready}, notif:${notifAfterKill.messages_ready})`,
    );
  });
});
