#!/usr/bin/env bash
# test-unit — the unit-suite duration gate (issue #57): go test ./... with the
# integration environment (TEST_*) cleared, failing when the wall time exceeds
# UNIT_SUITE_MAX_SECONDS (default 120s). The CI Go job runs this script in its
# "Unit suite" step with the service env cleared at step level; locally the
# unset below keeps .env-derived TEST_* values out either way.
#
# Integration helpers skip on an empty TEST_* env (their os.Getenv checks
# treat unset and empty alike), so the suite here is the unit-only baseline:
# domain tables, fakes, and error mapping — no database, Mongo, or MinIO.
set -euo pipefail
cd "$(dirname "$0")/../.."

unset TEST_DATABASE_URL TEST_MONGO_URI TEST_S3_ENDPOINT \
	TEST_S3_ACCESS_KEY TEST_S3_SECRET_KEY TEST_RABBITMQ_URL TEST_REDIS_URL

max="${UNIT_SUITE_MAX_SECONDS:-120}"

start=$(date +%s)
go test ./... -count=1
end=$(date +%s)
elapsed=$((end - start))

echo "test-unit: unit suite took ${elapsed}s (budget ${max}s)"
if [ "$elapsed" -gt "$max" ]; then
	echo "test-unit: FAIL — unit suite took ${elapsed}s, over the ${max}s budget (UNIT_SUITE_MAX_SECONDS)" >&2
	exit 1
fi
echo "test-unit: PASS — unit suite within the ${max}s budget (UNIT_SUITE_MAX_SECONDS)"
