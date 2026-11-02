#!/usr/bin/env bash
# demo-story — reset the demo tenant, then idempotently install the seeded
# story fixtures for the 5-minute demo script (issue #63, PRD §22;
# docs/demo-script.md is the presenter runbook this script backs).
#
# Usage: scripts/dev/demo-story.sh          # reset + install  (make demo)
#        scripts/dev/demo-story.sh --reset  # reset only       (make demo-reset)
#
# What "reset" means here (explicit, per-tenant — never the onboarding
# tenants 1111…/2222… nor the e2e tenants 3333…/4444…):
#   PostgreSQL  every tenant-scoped procurement.* row of the demo tenant —
#              including the three persistent per-tenant counters
#              (requisition_counters, po_counters, grn_counters: without
#              them the lazy init keeps allocating PR/PO/GRN-000002…) —
#              plus outbox and decision_idempotency, and the demo users in
#              identity.users by email (cascades user_roles +
#              refresh_tokens). Child rows (items, lines, approval steps)
#              follow their parents' ON DELETE CASCADE.
#   MongoDB    deleteMany({tenant_id}) on events, audit_log, document_meta,
#              notifications, AND export_runs — export_runs is the run
#              history the demo's step 5 displays on /app/reports, and a
#              manual run upserts on {tenant_id, scheduled_for} (= the
#              event's occurred_at, unique per run), so without the delete
#              the second demo run would show two history rows.
#   NOT reset  MinIO object bytes (attachments/… and exports/…). Metadata
#              is gone so the objects are orphans the pending-reaper never
#              collects (it only reaps pending declarations) — accepted
#              limitation, `make reset` is the nuclear option; see
#              docs/demo-script.md "What the reset does not do".
#
# What "install" writes (all idempotent — safe to re-run):
#   2 users via cmd/useradmin (ON CONFLICT upsert): the requester and a
#     SECOND user who approves — self-approval is forbidden for everyone,
#     admins included (docs/rbac.md), so one user alone cannot run the
#     story. Both hold the admin role (every permission; PRD §22's
#     buyer/finance legs run under the same logins).
#   1 vendor through the gateway (409 vendor_duplicate_code tolerated on
#     replays) — the vendors page is a read-only list with no create form,
#     so the vendor must exist before the curtain rises.
#   The single-step IDR approval chain (PUT /api/v1/approval-rules) — a
#     submitted PR is rejected with 412 approval_rules_missing without it.
#   Match tolerances qty 5% / price 2% (PUT /api/v1/match-settings) — the
#     non-default row is what the demo's tolerance step shows; 0%/0% is the
#     effective default while no row is stored.
#
# The story entities themselves (PR-000001, PO-000001, GRN-000001,
# INV-DEMO-001) are created live by the presenter through the UI — that is
# the demo. Passwords below are local fixture credentials in the documented
# onboarding `demo-password` shape, not secrets.
set -euo pipefail
cd "$(dirname "$0")/../.."

mode="install"
if [ "${1:-}" = "--reset" ]; then
	mode="reset"
elif [ -n "${1:-}" ]; then
	echo "usage: $0 [--reset]" >&2
	exit 2
fi

# --- The seeded story's constants (deterministic across runs) ------------
TENANT_ID="55555555-5555-5555-5555-555555555555"
REQUESTER_EMAIL="demo-requester@story.example"
APPROVER_EMAIL="demo-approver@story.example"
DEMO_PASSWORD="demo-password"
VENDOR_PAYLOAD='{"code":"DEMO-VND-01","name":"PT Demo Makmur","contact_email":"sales@demo-makmur.example","tax_id":"05.555.555.5-055.555"}'
APPROVAL_RULES_PAYLOAD='{"rules":[{"currency":"IDR","step_no":1,"role_required":"approver","threshold_cents":0}]}'
MATCH_SETTINGS_PAYLOAD='{"qty_pct":5,"price_pct":2}'

# --- Local config: .env values win, compose defaults otherwise -----------
env_value() { sed -n "s/^$1=//p" .env 2>/dev/null | tail -1; }
POSTGRES_USER="$(env_value POSTGRES_USER)"; POSTGRES_USER="${POSTGRES_USER:-eprocurement}"
POSTGRES_DB="$(env_value POSTGRES_DB)"; POSTGRES_DB="${POSTGRES_DB:-eprocurement}"
MONGO_ROOT_USER="$(env_value MONGO_ROOT_USER)"; MONGO_ROOT_USER="${MONGO_ROOT_USER:-eprocurement}"
MONGO_ROOT_PASSWORD="$(env_value MONGO_ROOT_PASSWORD)"; MONGO_ROOT_PASSWORD="${MONGO_ROOT_PASSWORD:-eprocurement}"
MONGO_DB="$(env_value MONGO_DB)"; MONGO_DB="${MONGO_DB:-eprocurement}"
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"

# cmd/useradmin runs on the host and needs the compose database; the same
# .env passthrough the Makefile's e2e target uses (an empty value falls
# back to the compose default, the fixtures.global-setup.ts pattern).
DATABASE_URL="${DATABASE_URL:-$(env_value DATABASE_URL)}"
DATABASE_URL="${DATABASE_URL:-postgres://eprocurement:eprocurement@localhost:5432/eprocurement?sslmode=disable}"

log() { printf 'demo-story: %s\n' "$*" >&2; }

# --- Preflight: the compose stack must be up (gateway healthy) ----------
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

reset_demo_tenant() {
	log "resetting tenant $TENANT_ID (PostgreSQL: procurement + identity; MongoDB: 5 collections)"
	docker compose exec -T postgres psql \
		-v ON_ERROR_STOP=1 -q \
		-U "$POSTGRES_USER" -d "$POSTGRES_DB" \
		-v tenant="$TENANT_ID" \
		-v user_a="$REQUESTER_EMAIL" \
		-v user_b="$APPROVER_EMAIL" <<'SQL'
BEGIN;
-- Children without their own tenant_id (invoice_lines, goods_receipt_items,
-- po_items, requisition_items, approval_steps) cascade with their parents.
DELETE FROM procurement.invoices             WHERE tenant_id = :'tenant';
DELETE FROM procurement.goods_receipts       WHERE tenant_id = :'tenant';
DELETE FROM procurement.purchase_orders      WHERE tenant_id = :'tenant';
DELETE FROM procurement.decision_idempotency WHERE tenant_id = :'tenant';
DELETE FROM procurement.requisitions         WHERE tenant_id = :'tenant';
-- The three persistent per-tenant counters: without these the lazy init
-- (max(existing number)) keeps the sequence where the last run left it.
DELETE FROM procurement.requisition_counters WHERE tenant_id = :'tenant';
DELETE FROM procurement.po_counters          WHERE tenant_id = :'tenant';
DELETE FROM procurement.grn_counters         WHERE tenant_id = :'tenant';
DELETE FROM procurement.approval_rules       WHERE tenant_id = :'tenant';
DELETE FROM procurement.match_settings       WHERE tenant_id = :'tenant';
DELETE FROM procurement.outbox               WHERE tenant_id = :'tenant';
DELETE FROM procurement.vendors              WHERE tenant_id = :'tenant';
-- Demo users by email (never by tenant: the onboarding/e2e tenants are
-- untouched); user_roles and refresh_tokens cascade.
DELETE FROM identity.users WHERE email IN (:'user_a', :'user_b');
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
	log "reset done — counters freed, so the story starts again at PR-000001 / PO-000001 / GRN-000001"
}

create_user() { # $1 email, $2 display name
	DATABASE_URL="$DATABASE_URL" go run ./services/identity/cmd/useradmin \
		-email "$1" -password "$DEMO_PASSWORD" \
		-tenant-id "$TENANT_ID" -role admin -name "$2" >/dev/null
	log "user ready: $1 (admin, tenant $TENANT_ID)"
}

install_story_fixtures() {
	create_user "$REQUESTER_EMAIL" "Demo Requester"
	create_user "$APPROVER_EMAIL" "Demo Approver"

	login_body='{"email":"'"$REQUESTER_EMAIL"'","password":"'"$DEMO_PASSWORD"'"}'
	login_json="$(curl -sf -X POST "$GATEWAY_URL/api/v1/auth/login" \
		-H 'content-type: application/json' -d "$login_body")" || {
		log "gateway login failed for $REQUESTER_EMAIL"
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
	201) log "vendor created: DEMO-VND-01 (PT Demo Makmur)" ;;
	409) log "vendor present: DEMO-VND-01 (idempotent replay)" ;;
	*) log "vendor create answered HTTP $status"; exit 1 ;;
	esac

	curl -sf -X PUT "$GATEWAY_URL/api/v1/approval-rules" \
		-H "authorization: Bearer $token" -H 'content-type: application/json' \
		-d "$APPROVAL_RULES_PAYLOAD" >/dev/null || {
		log "PUT /api/v1/approval-rules failed"
		exit 1
	}
	log "approval rules installed: 1 step, IDR, role approver, threshold 0"

	curl -sf -X PUT "$GATEWAY_URL/api/v1/match-settings" \
		-H "authorization: Bearer $token" -H 'content-type: application/json' \
		-d "$MATCH_SETTINGS_PAYLOAD" >/dev/null || {
		log "PUT /api/v1/match-settings failed"
		exit 1
	}
	log "match tolerances installed: qty 5% / price 2%"

	log "install summary:"
	log "  tenant      $TENANT_ID"
	log "  requester   $REQUESTER_EMAIL (password: $DEMO_PASSWORD)"
	log "  approver    $APPROVER_EMAIL (password: $DEMO_PASSWORD)"
	log "  vendor      DEMO-VND-01 — PT Demo Makmur (active)"
	log "  rules       IDR step 1: role approver, threshold 0"
	log "  tolerance   qty 5% / price 2%"
	log "  next numbers PR-000001 / PO-000001 / GRN-000001 (counters reset)"
	log "  open docs/demo-script.md and run the 5-minute story"
}

wait_for_gateway
reset_demo_tenant
if [ "$mode" = "install" ]; then
	install_story_fixtures
else
	log "reset-only mode (make demo-reset) — no fixtures installed"
fi
