// Local compose-stack lifecycle controls for the #60 resilience spec. The
// spec's crash injection is REAL infrastructure surgery, not a mock: the
// worker container is stopped gracefully (SIGTERM — in-flight handlers
// settle, the outbox keeps accumulating), SIGKILLed mid-flow
// (`docker compose kill worker`), and started again — the same commands a
// operator would run for PRD §22 step 6's manual demo (docs/demo-script.md
// step 6). mongosh runs inside the compose mongo container so the spec can
// assert the projection stores directly (count == distinct event_id).
//
// All commands run from the repo root so `docker compose` sees compose.yaml;
// the compose project name is pinned there (`name: eprocurement`).
import { execFile } from "node:child_process";
import { resolve } from "node:path";
import { promisify } from "node:util";

import { MONGO_DB, MONGO_ROOT_PASSWORD, MONGO_ROOT_USER } from "./fixtures";
import { DOCUMENTS_HISTORY_QUEUE, queueStats, waitForQueue } from "./rabbitmq";

const run = promisify(execFile);

// Playwright transpiles the support files to CommonJS (no import.meta
// shim in its loader), so __dirname is the portable way to the repo root.
const repoRoot = resolve(__dirname, "..", "..");

// compose runs one docker compose subcommand and returns its stdout. The
// timeout bounds a wedged docker CLI (Docker Desktop startup, slow CI).
export async function compose(args: string[], timeoutMs = 90_000): Promise<string> {
  const { stdout } = await run("docker", ["compose", ...args], {
    cwd: repoRoot,
    timeout: timeoutMs,
    maxBuffer: 16 * 1024 * 1024,
  });
  return stdout;
}

// stopWorkerGracefully stops the worker with SIGTERM (docker compose stop):
// both worker halves settle their in-flight work and return, unacknowledged
// deliveries return to their queues, and nothing publishes or consumes
// afterwards — the state PRD §14.8's "worker down" row describes, with the
// outbox as the durability backstop.
export async function stopWorkerGracefully(): Promise<void> {
  await compose(["stop", "worker"]);
  // The container is gone; the broker drops its channels. Wait until both
  // projection queues report zero consumers so the backlog phase starts
  // from a provably detached broker state.
  await waitForQueue(DOCUMENTS_HISTORY_QUEUE, (s) => s.consumers === 0, 30_000);
}

// killWorker SIGKILLs the worker container — `docker compose kill` with the
// signal pinned explicitly. Whatever delivery was mid-handler dies with the
// process and the broker returns it (and the consumer's buffered-but-
// unacknowledged deliveries) to the queue: the at-least-once redelivery the
// consumers must dedupe (PRD §14.8 "duplicate delivery" row).
export async function killWorker(): Promise<void> {
  await compose(["kill", "-s", "SIGKILL", "worker"]);
}

// brokerQueueDepths reads LIVE queue depths straight from the broker's
// queue processes (rabbitmqctl inside the compose rabbitmq container).
// The management HTTP API aggregates queue stats on a coarse interval, so
// it is the right witness for STABLE states (a settled backlog) but not
// for observing the sub-second in-flight state — the crash gate below
// needs the live view.
export async function brokerQueueDepths(): Promise<Map<string, { ready: number; unacked: number }>> {
  const out = await compose([
    "exec",
    "-T",
    "rabbitmq",
    "rabbitmqctl",
    "list_queues",
    "name",
    "messages_ready",
    "messages_unacknowledged",
    "--no-table-headers",
  ]);
  const depths = new Map<string, { ready: number; unacked: number }>();
  for (const line of out.split(/\r?\n/)) {
    const [name, ready, unacked] = line.trim().split(/\s+/);
    if (name) {
      depths.set(name, { ready: Number(ready), unacked: Number(unacked) });
    }
  }
  return depths;
}

// mongoFsyncLock/unlock pin the worker mid-flow for the #60 crash gate.
// Rationale (measured on this stack, see the dev note): the worker is one
// binary — its consumer settles each delivery within milliseconds of the
// publisher publishing it, so a healthy queue never holds depth and the
// in-flight window (unacknowledged deliveries) exists only for ~100 ms per
// 2 s publisher tick; an observation-gated bare SIGKILL races the docker
// CLI's latency against that window and cannot be made deterministic.
// fsyncLock — mongod's backup write lock, PRD §14.8's "Mongo down" row
// realized as a stalled projection store — blocks the consumer's Apply
// mid-handler, so its unacknowledged deliveries (and the ready depth the
// publisher keeps adding) become a STABLE, re-readable state the gate can
// observe before the kill. The lock is always released right after the
// kill (and best-effort in the spec's afterAll).
export async function mongoFsyncLock(): Promise<void> {
  await mongoEval("JSON.stringify(db.fsyncLock())");
}

export async function mongoFsyncUnlock(): Promise<void> {
  await mongoEval("JSON.stringify(db.fsyncUnlock())");
}

// startWorker starts the (stopped or killed) worker container again; boot
// reconnects the publisher and both consumer runners.
export async function startWorker(): Promise<void> {
  await compose(["start", "worker"]);
}

// ensureWorkerRunning is the self-healing preamble/epilogue: if the
// projection consumer is not attached (worker stopped, killed by a failed
// previous attempt, or still booting), start the container and wait until
// the consumer registers on documents.history. `docker compose start` on an
// already-running container is a no-op, so calling it unconditionally is
// safe — the consumer check keeps an early-boot window honest.
export async function ensureWorkerRunning(timeoutMs = 90_000): Promise<void> {
  const stats = await queueStats(DOCUMENTS_HISTORY_QUEUE).catch(() => null);
  if (stats && stats.consumers > 0) {
    return;
  }
  await startWorker();
  await waitForQueue(DOCUMENTS_HISTORY_QUEUE, (s) => s.consumers > 0, timeoutMs);
}

// mongoEval runs one JavaScript expression inside the compose mongo
// container's mongosh and returns its parsed JSON value. The eval script
// must JSON.stringify its result. Credentials mirror compose's
// MONGO_INITDB_ROOT_* interpolation (the local-dev fixture defaults).
export async function mongoEval(js: string): Promise<unknown> {
  const stdout = await compose([
    "exec",
    "-T",
    "mongo",
    "mongosh",
    "--quiet",
    "-u",
    MONGO_ROOT_USER,
    "-p",
    MONGO_ROOT_PASSWORD,
    "--authenticationDatabase",
    "admin",
    MONGO_DB,
    "--eval",
    js,
  ]);
  const trimmed = stdout.trim();
  try {
    return JSON.parse(trimmed);
  } catch {
    throw new Error(`mongosh did not answer JSON: ${trimmed.slice(0, 200)}`);
  }
}
