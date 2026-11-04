// RabbitMQ management API client for the #60 resilience spec: the broker's
// own observability surface (compose's rabbitmq:3.13-management image,
// port 15672) is the authoritative witness for "events are queued durably
// with no consumers" — the durable-backlog proof PRD §14.8 describes
// ("queue retains messages"). The spec gates its SIGKILL on an observed
// queue depth here, so the crash is deterministic rather than a sleep.
import { RABBITMQ_MANAGEMENT_URL, RABBITMQ_PASSWORD, RABBITMQ_USER } from "./fixtures";

// The worker topology's queue names (services/worker/topology/topology.go):
// documents.history binds `#` (every event), notifications binds the
// actor-facing routing-key subset (pr.submitted, pr.approved, …).
export const DOCUMENTS_HISTORY_QUEUE = "documents.history";
export const NOTIFICATIONS_QUEUE = "notifications";
export const DLQ_QUEUE = "procurement.dlq";

export type QueueStats = {
  name: string;
  // `messages` counts ready + unacknowledged; `messages_ready` is the
  // durable backlog that survives a consumer crash (unacknowledged
  // deliveries return to ready once the killed consumer's channel closes).
  messages: number;
  messages_ready: number;
  messages_unacknowledged: number;
  consumers: number;
};

// queueStats reads one queue's stats from the management API. The default
// vhost "/" is URL-encoded as %2F in the management API's path convention.
export async function queueStats(queue: string): Promise<QueueStats> {
  const auth = Buffer.from(`${RABBITMQ_USER}:${RABBITMQ_PASSWORD}`).toString("base64");
  const res = await fetch(
    `${RABBITMQ_MANAGEMENT_URL}/api/queues/%2F/${encodeURIComponent(queue)}`,
    { headers: { authorization: `Basic ${auth}` } },
  );
  if (!res.ok) {
    throw new Error(`rabbitmq management API answered ${res.status} for queue ${queue}`);
  }
  const body = (await res.json()) as Partial<QueueStats>;
  return {
    name: queue,
    messages: body.messages ?? 0,
    messages_ready: body.messages_ready ?? 0,
    messages_unacknowledged: body.messages_unacknowledged ?? 0,
    consumers: body.consumers ?? 0,
  };
}

// waitForQueue polls a queue's stats until predicate passes, returning the
// passing snapshot; on timeout it fails with the last observed snapshot so
// the test report shows what the broker actually held.
export async function waitForQueue(
  queue: string,
  predicate: (stats: QueueStats) => boolean,
  timeoutMs = 30_000,
  intervalMs = 200,
): Promise<QueueStats> {
  const deadline = Date.now() + timeoutMs;
  let last: QueueStats | null = null;
  let lastError: unknown = null;
  while (Date.now() < deadline) {
    try {
      last = await queueStats(queue);
      if (predicate(last)) {
        return last;
      }
    } catch (err) {
      lastError = err;
    }
    await new Promise((resolve) => setTimeout(resolve, intervalMs));
  }
  const seen = last ? JSON.stringify(last) : `no reading (last error: ${String(lastError)})`;
  throw new Error(`queue ${queue} never satisfied the wait predicate within ${timeoutMs}ms — last: ${seen}`);
}

// waitForAllQueues polls every queue's stats in one pass until predicate
// passes over the whole snapshot — the gate for "both projection queues
// hold unacknowledged deliveries SIMULTANEOUSLY" (the in-flight state the
// #60 crash freezes), which per-queue waits cannot express.
export async function waitForAllQueues(
  queues: string[],
  predicate: (stats: QueueStats[]) => boolean,
  timeoutMs = 45_000,
  intervalMs = 100,
): Promise<QueueStats[]> {
  const deadline = Date.now() + timeoutMs;
  let last: QueueStats[] | null = null;
  let lastError: unknown = null;
  while (Date.now() < deadline) {
    try {
      last = await Promise.all(queues.map((q) => queueStats(q)));
      if (predicate(last)) {
        return last;
      }
    } catch (err) {
      lastError = err;
    }
    await new Promise((resolve) => setTimeout(resolve, intervalMs));
  }
  const seen = last ? last.map((s) => JSON.stringify(s)).join(" ") : `no reading (last error: ${String(lastError)})`;
  throw new Error(`queues {${queues.join(", ")}} never satisfied the wait predicate within ${timeoutMs}ms — last: ${seen}`);
}
