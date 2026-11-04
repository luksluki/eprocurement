// The #61 HTTP load test: a mixed read/write scenario against the gateway
// of a running compose stack. This file is driven by scripts/dev/loadtest.sh
// (`make loadtest`), which drains the projection queues, resets the
// dedicated load tenant (7777…), installs the fixture user/vendor/approval
// rules, then runs this script; the measured results and the runbook live
// in docs/perf.md §9.
//
// Scenario (short by design — issue #61 "keep the scenario short"):
//   ~80% reads  GET /api/v1/vendors, /api/v1/requisitions,
//               /api/v1/purchase-orders, /api/v1/dashboard/summary,
//               /api/v1/events (page 1) — evenly split;
//   ~20% writes POST /api/v1/requisitions (draft) followed by
//               POST /api/v1/requisitions/:id/submit — submit appends the
//               pr.submitted outbox row inside its transaction
//               (services/procurement/requisition/store.go), so every write
//               iteration also drives the async pipeline (publisher →
//               RabbitMQ → worker → Mongo projections).
//
// Login happens exactly ONCE per run, in setup(), and the token is shared
// by every VU: identity's login limiter allows 5 attempts/min per
// email+address (services/identity/config/config.go) and the gateway's
// 30/min per IP (.env.example), so per-VU logins would measure the limiter
// rather than the API. The access token lives 15 minutes (AccessTTL,
// services/identity/auth/service.go) and the scenario deliberately does not
// refresh, so the run duration is capped below the TTL — a longer DURATION
// fails fast in the init context with an explicit message.
//
// Scale caveat (docs/perf.md §9): this measures the HTTP pipeline overhead
// (gateway → gRPC → service → PostgreSQL/Mongo) on a SMALL dataset — the
// store-level cost of large data is #56's territory (docs/perf.md §2–§3).
//
// Environment:
//   BASE_URL      gateway base URL          (default http://localhost:8080)
//   LOAD_EMAIL    fixture user email        (installed by the wrapper)
//   LOAD_PASSWORD fixture user password     (installed by the wrapper)
//   VUS           concurrent virtual users  (default 10)
//   DURATION      steady-state duration     (default 2m; hard cap 14m)
//   SUMMARY_FILE  JSON summary export path  (default
//                 scripts/loadtest/summary-latest.json, relative to the
//                 working directory — gitignored, regenerated per run)

import http from 'k6/http';
import { check } from 'k6';
import { Counter, Trend } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const EMAIL = __ENV.LOAD_EMAIL;
const PASSWORD = __ENV.LOAD_PASSWORD;
const VUS = Number(__ENV.VUS || 10);
const DURATION = __ENV.DURATION || '2m';

// One access token, minted in setup() and shared by every VU: 15 min
// (AccessTTL, services/identity/auth/service.go:27). setup() itself takes
// seconds, so runs longer than ~14 minutes would end with 401s as the
// token expires mid-run — fail fast instead, with the reason.
const ACCESS_TTL_MIN = 15;
const MAX_DURATION_MIN = 14;

// The single-step IDR approval chain (the onboarding §5.1 / e2e fixture
// shape): without it submit answers 412 approval_rules_missing. PUT is
// idempotent, so setup() re-asserting it is safe even though the wrapper
// already installed it — the script stays self-contained this way.
const APPROVAL_RULES_BODY = JSON.stringify({
  rules: [{ currency: 'IDR', step_no: 1, role_required: 'approver', threshold_cents: 0 }],
});

// ~80% reads / ~20% writes. Each iteration either runs the two-call write
// chain or ONE read endpoint (reads split evenly: 16% of iterations each),
// so the weights converge over the run.
const WRITE_SHARE = 0.2;
const READS = [
  { slug: 'get_vendors', label: 'GET /api/v1/vendors', path: '/api/v1/vendors' },
  { slug: 'get_requisitions', label: 'GET /api/v1/requisitions', path: '/api/v1/requisitions' },
  { slug: 'get_purchase_orders', label: 'GET /api/v1/purchase-orders', path: '/api/v1/purchase-orders' },
  { slug: 'get_dashboard_summary', label: 'GET /api/v1/dashboard/summary', path: '/api/v1/dashboard/summary' },
  { slug: 'get_events', label: 'GET /api/v1/events', path: '/api/v1/events?limit=100' },
];
const WRITES = [
  { slug: 'post_requisitions', label: 'POST /api/v1/requisitions' },
  { slug: 'post_requisitions_submit', label: 'POST /api/v1/requisitions/:id/submit' },
];
const ENDPOINTS = [...READS, ...WRITES];

// Per-endpoint metrics are explicit Trend/Counter instances rather than
// group() sub-metrics: current k6 no longer emits `http_req_duration{group…}`
// rows in the end-of-test summary data, and explicit metrics are stable
// across versions. latency_* is a time Trend (ms), calls_/failures_ Counters
// give the per-endpoint throughput and error rate.
const latency = {};
const calls = {};
const failures = {};
for (const ep of ENDPOINTS) {
  latency[ep.slug] = new Trend(`latency_${ep.slug}`, true);
  calls[ep.slug] = new Counter(`calls_${ep.slug}`);
  failures[ep.slug] = new Counter(`failures_${ep.slug}`);
}

function record(slug, res, expect) {
  latency[slug].add(res.timings.duration);
  calls[slug].add(1);
  if (res.status !== expect) {
    failures[slug].add(1);
  }
}

function parseDurationMs(input) {
  const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s)?$/.exec(String(input).trim());
  if (!m || String(input).trim() === '') return NaN;
  const [, h, min, sec] = m;
  if (!h && !min && !sec) return NaN;
  return ((Number(h || 0) * 60 + Number(min || 0)) * 60 + Number(sec || 0)) * 1000;
}

const durationMs = parseDurationMs(DURATION);
if (!Number.isFinite(durationMs) || durationMs <= 0) {
  throw new Error(
    `DURATION "${DURATION}" is not a parseable duration — use e.g. "30s", "2m", "90s", "14m"`,
  );
}
if (durationMs > MAX_DURATION_MIN * 60 * 1000) {
  throw new Error(
    `DURATION "${DURATION}" exceeds the ${MAX_DURATION_MIN} minute ceiling: the shared setup() token lives ` +
      `${ACCESS_TTL_MIN} minutes (AccessTTL, services/identity/auth/service.go) and the scenario logs in exactly ` +
      `once by design (identity's login limiter is 5/min per email) — keep DURATION <= ${MAX_DURATION_MIN}m`,
  );
}

export const options = {
  vus: VUS,
  duration: DURATION,
  // The only gate (#61: numbers are documented, not latency-gated — the
  // perf.md §8 trend/gating follow-up stays open): fewer than 1% of
  // requests may fail (k6 counts 4xx/5xx as failed by default).
  thresholds: {
    http_req_failed: ['rate<0.01'],
  },
  summaryTrendStats: ['avg', 'min', 'med', 'p(95)', 'p(99)', 'max'],
  summaryTimeUnit: 'ms',
};

export function setup() {
  if (!EMAIL || !PASSWORD) {
    throw new Error('LOAD_EMAIL and LOAD_PASSWORD must be set (the wrapper installs the fixture user)');
  }
  // The single login of the run (see header): one identity rate-limit slot,
  // one gateway slot, one token for every VU.
  const login = http.post(
    `${BASE_URL}/api/v1/auth/login`,
    JSON.stringify({ email: EMAIL, password: PASSWORD }),
    { headers: { 'content-type': 'application/json' } },
  );
  if (login.status !== 200) {
    throw new Error(`login as ${EMAIL} failed: HTTP ${login.status} — ${String(login.body).slice(0, 200)}`);
  }
  const token = login.json('access_token');
  if (!token) {
    throw new Error('login response carried no access_token');
  }
  // Idempotent re-assert of the approval chain so submit never 412s even
  // when this script is run directly (the wrapper installed it too).
  const headers = { authorization: `Bearer ${token}`, 'content-type': 'application/json' };
  const rules = http.put(`${BASE_URL}/api/v1/approval-rules`, APPROVAL_RULES_BODY, { headers });
  if (rules.status !== 200) {
    throw new Error(`PUT /api/v1/approval-rules failed: HTTP ${rules.status} — ${String(rules.body).slice(0, 200)}`);
  }
  return { token: token };
}

function readEndpoint(headers, target) {
  const res = http.get(`${BASE_URL}${target.path}`, {
    headers,
    tags: { name: target.label },
  });
  record(target.slug, res, 200);
  check(res, { 'status 200': (r) => r.status === 200 });
}

function writeChain(headers) {
  const payload = JSON.stringify({
    currency: 'IDR',
    items: [
      {
        description: `k6 load test item — vu ${__VU} iter ${__ITER}`,
        qty: 1 + (__ITER % 3),
        unit_price_cents: 250000,
        category: 'it',
      },
    ],
  });
  const created = http.post(`${BASE_URL}/api/v1/requisitions`, payload, {
    headers: { ...headers, 'content-type': 'application/json' },
    tags: { name: 'POST /api/v1/requisitions' },
  });
  record('post_requisitions', created, 201);
  check(created, { 'status 201': (r) => r.status === 201 });
  if (created.status !== 201) return;
  const id = created.json('id');
  if (!id) return;
  const submitted = http.post(`${BASE_URL}/api/v1/requisitions/${id}/submit`, null, {
    headers,
    tags: { name: 'POST /api/v1/requisitions/:id/submit' },
  });
  record('post_requisitions_submit', submitted, 200);
  check(submitted, { 'status 200': (r) => r.status === 200 });
}

export default function (data) {
  const headers = { authorization: `Bearer ${data.token}` };
  const roll = Math.random();
  if (roll < WRITE_SHARE) {
    writeChain(headers);
    return;
  }
  // Reads split the remaining share evenly across the five endpoints.
  const slice = (roll - WRITE_SHARE) / (1 - WRITE_SHARE);
  const target = READS[Math.min(Math.floor(slice * READS.length), READS.length - 1)];
  readEndpoint(headers, target);
}

// handleSummary turns the per-endpoint metrics into the table docs/perf.md
// §9 records (median/p95/p99 in ms, throughput, error rate) and exports the
// raw metrics as JSON next to this script. Defining it replaces k6's
// default end-of-test summary.
export function handleSummary(data) {
  const rows = ENDPOINTS.map((ep) => {
    const dur = (data.metrics[`latency_${ep.slug}`] || {}).values || {};
    const call = (data.metrics[`calls_${ep.slug}`] || {}).values || {};
    const fail = (data.metrics[`failures_${ep.slug}`] || {}).values || {};
    return {
      name: ep.label,
      count: call.count || 0,
      rate: call.rate || 0,
      errorPct: call.count ? ((fail.count || 0) / call.count) * 100 : 0,
      med: dur.med,
      p95: dur['p(95)'],
      p99: dur['p(99)'],
    };
  });

  const fmt = (v) => (typeof v === 'number' ? v.toFixed(1) : String(v));
  const pad = (s, w) => String(s).padEnd(w);
  const padL = (s, w) => String(s).padStart(w);

  const nameW = Math.max('endpoint'.length, ...rows.map((r) => r.name.length));
  const lines = [];
  lines.push('');
  lines.push('loadtest: per-endpoint summary (ms; closed loop, no think time)');
  lines.push(
    `${pad('endpoint', nameW)}  ${padL('reqs', 7)}  ${padL('rate/s', 8)}  ${padL('err%', 6)}  ${padL('med', 8)}  ${padL('p95', 8)}  ${padL('p99', 8)}`,
  );
  for (const r of rows) {
    lines.push(
      `${pad(r.name, nameW)}  ${padL(r.count, 7)}  ${padL(r.rate.toFixed(1), 8)}  ${padL(r.errorPct.toFixed(2), 6)}  ${padL(fmt(r.med), 8)}  ${padL(fmt(r.p95), 8)}  ${padL(fmt(r.p99), 8)}`,
    );
  }
  const overallReqs = (data.metrics.http_reqs || {}).values || {};
  const overallFailed = (data.metrics.http_req_failed || {}).values || {};
  const overallDur = (data.metrics.http_req_duration || {}).values || {};
  const checks = (data.metrics.checks || {}).values || {};
  const iters = (data.metrics.iterations || {}).values || {};
  lines.push(
    `${pad('OVERALL', nameW)}  ${padL(overallReqs.count || 0, 7)}  ${padL((overallReqs.rate || 0).toFixed(1), 8)}  ${padL(((overallFailed.rate || 0) * 100).toFixed(2), 6)}  ${padL(fmt(overallDur.med), 8)}  ${padL(fmt(overallDur['p(95)']), 8)}  ${padL(fmt(overallDur['p(99)']), 8)}`,
  );
  lines.push('');
  lines.push(
    `loadtest: iterations=${iters.count || 0} checks_ok=${checks.rate !== undefined ? (checks.rate * 100).toFixed(1) + '%' : '?'} vus=${VUS} duration=${DURATION} base_url=${BASE_URL}`,
  );

  const summaryFile = __ENV.SUMMARY_FILE || 'scripts/loadtest/summary-latest.json';
  const payload = JSON.stringify(
    {
      run: { base_url: BASE_URL, vus: VUS, duration: DURATION, email: EMAIL },
      metrics: data.metrics,
    },
    null,
    2,
  );
  const out = { stdout: lines.join('\n') + '\n' };
  out[summaryFile] = payload;
  return out;
}
