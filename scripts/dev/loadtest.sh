#!/usr/bin/env bash
# loadtest — the #61 HTTP load test against the compose stack: reset the
# dedicated load tenant, install the fixtures, run the k6 mixed read/write
# scenario (scripts/loadtest/loadtest.js), then report per-endpoint latency,
# broker/outbox drain, and the tenant's row counts.
#
# Usage: scripts/dev/loadtest.sh    (or: make loadtest)
# Env:   LOADTEST_VUS       concurrent virtual users (default 10)
#        LOADTEST_DURATION  steady-state duration      (default 2m; the k6
#                           script hard-caps runs at 14m — the shared setup()
#                           token lives 15 min, services/identity/auth)
#        GATEWAY_URL        gateway base URL (default http://localhost:8080)
# Requires: k6 on PATH (https://grafana.com/docs/k6/latest/set-up/install/)
#           and the compose stack up (make compose-up).
#
# Reproducibility, in order (the "results reproducible" AC):
#   1. wait for the gateway (/healthz);
#   2. wait for the projection queues AND this tenant's pending outbox rows
#      to drain BEFORE wiping the tenant — after a load run the broker and
#      the outbox almost certainly still hold pr.submitted backlog (the
#      publisher moves 50 rows per 2 s tick), and wiping while the worker
#      still holds messages, or while a publisher batch is mid-flight, lets
#      events/notifications re-project into the freshly wiped tenant, so
#      run N+1 would measure on top of run N's backlog;
#   3. reset tenant 7777… (PostgreSQL procurement rows + the three counters
#      + outbox + decision_idempotency + identity.users by email, and the
#      five Mongo collections — the demo-story.sh per-tenant semantics);
#   4. install the fixtures: one admin user (cmd/useradmin), one active
#      vendor (409-tolerant replay), the single-step IDR approval chain;
#   5. drain-check again as the clean-start gate right before k6 starts
#      (the fixture vendor's vendor.created event must have settled);
#   6. run k6 while a background sampler records RabbitMQ queue depth,
#      pending outbox rows, and docker stats every 5 s;
#   7. wait for the run's own outbox/queue backlog to drain (the drain time
#      IS the async-pipeline bottleneck measurement), then print the
#      per-table row counts (the write lane's footprint) and the maxima the
#      sampler saw (the bottleneck evidence docs/perf.md §9 records).
#
# The load tenant is deliberately NOT the onboarding tenants (1111…/2222…),
# the e2e tenants (3333…/4444…), the demo-story tenant (5555…), or the
# resilience tenant (6666…). Password below is a local fixture credential
# in the documented onboarding `demo-password` shape, not a secret.
set -euo pipefail
cd "$(dirname "$0")/../.."

# --- The load test's constants (deterministic across runs) ---------------
TENANT_ID="77777777-7777-7777-7777-777777777777"
LOAD_EMAIL="loadtest-admin@load.example"
LOAD_PASSWORD="load-password"
VENDOR_PAYLOAD='{"code":"LOAD-VND-01","name":"PT Load Test Supplies","contact_email":"sales@load-test.example","tax_id":"07.777.777.7-077.777"}'
APPROVAL_RULES_PAYLOAD='{"rules":[{"currency":"IDR","step_no":1,"role_required":"approver","threshold_cents":0}]}'

VUS="${LOADTEST_VUS:-10}"
DURATION="${LOADTEST_DURATION:-2m}"
# Budget (seconds) for draining this tenant's outbox/queue backlog before
# the wipe and after the run. The publisher moves 50 rows per 2 s tick
# (WORKER_POLL_INTERVAL/WORKER_BATCH_SIZE defaults, PRD §16) = 25 rows/s,
# and a 2 m run's write lane produces >10k pr.submitted rows, so a full
# drain legitimately takes minutes — 60 s (the demo-story budget, ~5 events)
# is nowhere near enough here.
DRAIN_BUDGET="${LOADTEST_DRAIN_BUDGET:-600}"

# --- Local config: .env values win, compose defaults otherwise -----------
env_value() { sed -n "s/^$1=//p" .env 2>/dev/null | tail -1; }
POSTGRES_USER="$(env_value POSTGRES_USER)"; POSTGRES_USER="${POSTGRES_USER:-eprocurement}"
POSTGRES_DB="$(env_value POSTGRES_DB)"; POSTGRES_DB="${POSTGRES_DB:-eprocurement}"
MONGO_ROOT_USER="$(env_value MONGO_ROOT_USER)"; MONGO_ROOT_USER="${MONGO_ROOT_USER:-eprocurement}"
MONGO_ROOT_PASSWORD="$(env_value MONGO_ROOT_PASSWORD)"; MONGO_ROOT_PASSWORD="${MONGO_ROOT_PASSWORD:-eprocurement}"
MONGO_DB="$(env_value MONGO_DB)"; MONGO_DB="${MONGO_DB:-eprocurement}"
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"

# cmd/useradmin runs on the host and needs the compose database (the same
# .env passthrough the Makefile's e2e/demo targets use).
DATABASE_URL="${DATABASE_URL:-$(env_value DATABASE_URL)}"
DATABASE_URL="${DATABASE_URL:-postgres://eprocurement:eprocurement@localhost:5432/eprocurement?sslmode=disable}"

log() { printf 'loadtest: %s\n' "$*" >&2; }

# --- Preflight: k6, then the compose stack (gateway healthy) -------------
if ! command -v k6 >/dev/null 2>&1; then
	log "k6 not found — install it first (https://grafana.com/docs/k6/latest/set-up/install/),"
	log "  e.g. 'scoop install k6' / 'choco install k6' on Windows, 'brew install k6' on macOS"
	exit 1
fi
K6_VERSION="$(k6 version)"

wait_for_gateway() {
	local deadline=$((SECONDS + 60))
	while [ "$SECONDS" -lt "$deadline" ]; do
		if curl -sf "$GATEWAY_URL/healthz" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	log "gateway at $GATEWAY_URL never became healthy — run: make compose-up"
	exit 1
}

# Ported from demo-story.sh: block until the two Mongo-projection queues are
# empty (or 60 s pass). Called before the wipe (a leftover backlog must not
# re-project into the wiped tenant); the pre-k6 clean-start gate is the
# stricter wait_outbox_drained (queues AND pending outbox rows), which also
# covers the queues.
wait_projection_queues_drained() {
	for _ in $(seq 1 30); do
		depth=$(docker compose exec -T rabbitmq rabbitmqctl list_queues name messages 			--no-table-headers 2>/dev/null 			| awk '$1 == "documents.history" || $1 == "notifications" { n += $2 } END { print n + 0 }')
		if [ "${depth:-1}" -eq 0 ]; then
			return 0
		fi
		sleep 2
	done
	log "projection queues still hold ${depth:-?} message(s) after 60s — the worker looks down; run: make compose-up, then retry"
	return 1
}

reset_load_tenant() {
	wait_projection_queues_drained || exit 1
	# The queue check above covers what the worker already holds; the outbox
	# check below closes the last race — a publisher batch SELECTed before
	# the wipe would still publish its ~50 rows into the freshly wiped
	# tenant. Both must be zero before the delete runs.
	wait_outbox_drained "$DRAIN_BUDGET" "pre-wipe" || exit 1
	log "resetting tenant $TENANT_ID (PostgreSQL: procurement + identity; MongoDB: 5 collections)"
	docker compose exec -T postgres psql \
		-v ON_ERROR_STOP=1 -q \
		-U "$POSTGRES_USER" -d "$POSTGRES_DB" \
		-v tenant="$TENANT_ID" \
		-v user_a="$LOAD_EMAIL" <<'SQL'
BEGIN;
-- Children without their own tenant_id (invoice_lines, goods_receipt_items,
-- po_items, requisition_items, approval_steps) cascade with their parents.
DELETE FROM procurement.invoices             WHERE tenant_id = :'tenant';
DELETE FROM procurement.goods_receipts       WHERE tenant_id = :'tenant';
DELETE FROM procurement.purchase_orders      WHERE tenant_id = :'tenant';
DELETE FROM procurement.decision_idempotency WHERE tenant_id = :'tenant';
DELETE FROM procurement.requisitions         WHERE tenant_id = :'tenant';
-- The three persistent per-tenant counters: without these the lazy init
-- keeps allocating PR-0000N… where the last run left it.
DELETE FROM procurement.requisition_counters WHERE tenant_id = :'tenant';
DELETE FROM procurement.po_counters          WHERE tenant_id = :'tenant';
DELETE FROM procurement.grn_counters         WHERE tenant_id = :'tenant';
DELETE FROM procurement.approval_rules       WHERE tenant_id = :'tenant';
DELETE FROM procurement.match_settings       WHERE tenant_id = :'tenant';
DELETE FROM procurement.outbox               WHERE tenant_id = :'tenant';
DELETE FROM procurement.vendors              WHERE tenant_id = :'tenant';
-- The load user by email (never by tenant: the onboarding/e2e/demo tenants
-- are untouched); user_roles and refresh_tokens cascade.
DELETE FROM identity.users WHERE email = :'user_a';
COMMIT;
SQL
	docker compose exec -T mongo mongosh --quiet \
		-u "$MONGO_ROOT_USER" -p "$MONGO_ROOT_PASSWORD" --authenticationDatabase admin \
		"$MONGO_DB" --eval "
db.events.deleteMany({tenant_id: '$TENANT_ID'});
db.audit_log.deleteMany({tenant_id: '$TENANT_ID'});
db.document_meta.deleteMany({tenant_id: '$TENANT_ID'});
db.notifications.deleteMany({tenant_id: '$TENANT_ID'});
db.export_runs.deleteMany({tenant_id: '$TENANT_ID'});
" >/dev/null
	log "reset done — the run starts from an empty tenant (counters freed, next PR is PR-000001)"
}

install_fixtures() {
	DATABASE_URL="$DATABASE_URL" go run ./services/identity/cmd/useradmin \
		-email "$LOAD_EMAIL" -password "$LOAD_PASSWORD" \
		-tenant-id "$TENANT_ID" -role admin -name "Load Test Admin" >/dev/null
	log "user ready: $LOAD_EMAIL (admin, tenant $TENANT_ID)"

	login_body='{"email":"'"$LOAD_EMAIL"'","password":"'"$LOAD_PASSWORD"'"}'
	login_json="$(curl -sf -X POST "$GATEWAY_URL/api/v1/auth/login" \
		-H 'content-type: application/json' -d "$login_body")" || {
		log "gateway login failed for $LOAD_EMAIL"
		exit 1
	}
	token="$(printf '%s' "$login_json" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')"
	if [ -z "$token" ]; then
		log "login response carried no access_token"
		exit 1
	fi

	status="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$GATEWAY_URL/api/v1/vendors" \
		-H "authorization: Bearer $token" -H 'content-type: application/json' \
		-d "$VENDOR_PAYLOAD")"
	case "$status" in
	201) log "vendor created: LOAD-VND-01 (PT Load Test Supplies)" ;;
	409) log "vendor present: LOAD-VND-01 (idempotent replay)" ;;
	*) log "vendor create answered HTTP $status"; exit 1 ;;
	esac

	curl -sf -X PUT "$GATEWAY_URL/api/v1/approval-rules" \
		-H "authorization: Bearer $token" -H 'content-type: application/json' \
		-d "$APPROVAL_RULES_PAYLOAD" >/dev/null || {
		log "PUT /api/v1/approval-rules failed"
		exit 1
	}
	log "approval rules installed: 1 step, IDR, role approver, threshold 0"
}

# --- During-run sampler: queue depth + pending outbox every 5 s, docker
# stats every 15 s — the bottleneck evidence (maxima) reported at the end.
queue_depth() {
	docker compose exec -T rabbitmq rabbitmqctl list_queues name messages 		--no-table-headers 2>/dev/null 		| awk '$1 == "documents.history" || $1 == "notifications" { n += $2 } END { print n + 0 }'
}

outbox_pending() {
	docker compose exec -T postgres psql -qtA \
		-U "$POSTGRES_USER" -d "$POSTGRES_DB" \
		-c "SELECT count(*) FROM procurement.outbox WHERE tenant_id = '$TENANT_ID' AND published_at IS NULL"
}

# wait_outbox_drained blocks until BOTH projection queues are empty AND this
# tenant's outbox has no pending row (or the budget passes; $1 = budget
# seconds, $2 = label for messages). Echoes the elapsed seconds on success.
wait_outbox_drained() {
	local started=$SECONDS
	local deadline=$((SECONDS + $1))
	local label="${2:-drain}"
	local depth=1 pending=1
	while [ "$SECONDS" -lt "$deadline" ]; do
		depth="$(queue_depth)"
		pending="$(outbox_pending)"
		if [ "$depth" -eq 0 ] && [ "$pending" -eq 0 ]; then
			echo $((SECONDS - started))
			return 0
		fi
		sleep 2
	done
	log "$label: backlog still holds queue depth=$depth, pending outbox=$pending after ${1}s —"
	log "  the publisher moves 50 rows per 2s tick (25 rows/s); raise LOADTEST_DRAIN_BUDGET"
	log "  or shorten LOADTEST_DURATION, then retry"
	return 1
}

samples="$(mktemp)"
cpu_samples="$(mktemp)"
sample_stop="$(mktemp)"
cleanup() { rm -f "$samples" "$cpu_samples" "$sample_stop"; }
trap cleanup EXIT

sample_loop() {
	local i=0
	while [ -f "$sample_stop" ]; do
		printf '%s %s %s\n' "$(date +%s)" "$(queue_depth)" "$(outbox_pending)" >>"$samples"
		if [ $((i % 3)) -eq 0 ]; then
			docker stats --no-stream --format '{{.Name}} {{.CPUPerc}} {{.MemUsage}}' >>"$cpu_samples" 2>/dev/null || true
		fi
		i=$((i + 1))
		sleep 5
	done
}

report_row_counts() {
	log "row counts (tenant $TENANT_ID):"
	docker compose exec -T postgres psql -qtA \
		-U "$POSTGRES_USER" -d "$POSTGRES_DB" \
		-v tenant="$TENANT_ID" <<'SQL' | awk -F'|' '{ printf "loadtest:   %s: %s\n", $1, $2 }' >&2
SELECT 'vendors|'           || count(*) FROM procurement.vendors      WHERE tenant_id = :'tenant'
UNION ALL
SELECT 'requisitions|'      || count(*) FROM procurement.requisitions WHERE tenant_id = :'tenant'
UNION ALL
SELECT 'requisition_items|' || count(*) FROM procurement.requisition_items ri
	JOIN procurement.requisitions r ON r.id = ri.requisition_id WHERE r.tenant_id = :'tenant'
UNION ALL
SELECT 'approval_steps|'    || count(*) FROM procurement.approval_steps st
	JOIN procurement.requisitions r ON r.id = st.requisition_id WHERE r.tenant_id = :'tenant'
UNION ALL
SELECT 'purchase_orders|'   || count(*) FROM procurement.purchase_orders WHERE tenant_id = :'tenant'
UNION ALL
SELECT 'outbox|'            || count(*) FROM procurement.outbox WHERE tenant_id = :'tenant';
SQL
	log "mongo projections:"
	docker compose exec -T mongo mongosh --quiet \
		-u "$MONGO_ROOT_USER" -p "$MONGO_ROOT_PASSWORD" --authenticationDatabase admin \
		"$MONGO_DB" --eval "
print('  events: ' + db.events.countDocuments({tenant_id: '$TENANT_ID'}));
print('  notifications: ' + db.notifications.countDocuments({tenant_id: '$TENANT_ID'}));
print('  audit_log: ' + db.audit_log.countDocuments({tenant_id: '$TENANT_ID'}));
"
}

# --- The run ---------------------------------------------------------------
wait_for_gateway
log "k6: $K6_VERSION — ${VUS} VUs x ${DURATION} against $GATEWAY_URL (drain budget ${DRAIN_BUDGET}s)"
reset_load_tenant
install_fixtures
wait_outbox_drained "$DRAIN_BUDGET" "clean-start" || exit 1
log "clean start: projection queues and outbox drained, tenant empty, fixtures installed"

touch "$sample_stop"
sample_loop &
sampler_pid=$!

k6_rc=0
BASE_URL="$GATEWAY_URL" LOAD_EMAIL="$LOAD_EMAIL" LOAD_PASSWORD="$LOAD_PASSWORD" \
	VUS="$VUS" DURATION="$DURATION" \
	k6 run scripts/loadtest/loadtest.js || k6_rc=$?

rm -f "$sample_stop"
wait "$sampler_pid" 2>/dev/null || true

max_depth="$(awk '{ if ($2 > m) m = $2 } END { print m + 0 }' "$samples")"
max_pending="$(awk '{ if ($3 > m) m = $3 } END { print m + 0 }' "$samples")"
log "during-run maxima: queue depth (documents.history+notifications) = $max_depth, pending outbox rows = $max_pending"
log "peak per-service CPU (docker stats, sampled every 15s):"
awk '{ cpu[$1] = ($2 + 0 > cpu[$1] ? $2 + 0 : cpu[$1]) } END { for (s in cpu) printf "  %s %.0f%%\n", s, cpu[s] }' "$cpu_samples" | sort >&2

# The post-run drain is itself the #61 bottleneck measurement: how long the
# async pipeline (publisher → RabbitMQ → worker → Mongo) needs to absorb the
# write lane's backlog, at the publisher's steady 50 rows / 2 s tick.
drain_secs=""
if drain_secs="$(wait_outbox_drained "$DRAIN_BUDGET" "post-run")"; then
	rate="n/a"
	if [ "$drain_secs" -gt 0 ] && [ "$max_pending" -gt 0 ]; then
		rate="$(awk -v p="$max_pending" -v s="$drain_secs" 'BEGIN { printf "%.1f", p / s }')"
	fi
	log "post-run backlog drained in ~${drain_secs}s (peak backlog $max_pending rows → ~${rate} rows/s sustained drain)"
else
	log "row counts below are taken with a backlog still present — projections are incomplete"
fi

report_row_counts
log "note: re-running back-to-back within a minute can trip identity's 5/min-per-email"
log "login limiter (this run logged in twice: fixture install + k6 setup) — space runs"
log "or set IDENTITY_LOGIN_RATE_PER_MIN in .env (the e2e/CI convention raises it to 60)"

if [ "$k6_rc" -ne 0 ]; then
	log "FAIL: k6 exited $k6_rc (threshold http_req_failed<1% or a setup/check failure)"
	exit "$k6_rc"
fi
log "PASS: load test completed (results and interpretation: docs/perf.md §9)"
