#!/usr/bin/env bash
# perf-budget — run the ~100k-row scale fixtures against the compose stack
# and report every measured shape's p95 against the budget its test enforces
# (issue #56; budgets live in the tests: 300 ms lists, 300 ms dashboard).
#
# Usage: scripts/dev/perf-budget.sh   (or: make perf-budget)
# Requires the compose PostgreSQL (make compose-up). The five fixtures
# serialize their bulk-load phases on the scalegate advisory lock, so a full
# run takes roughly 10 minutes; -timeout 30m covers the queueing.
set -euo pipefail
cd "$(dirname "$0")/../.."

# Same .env fallback the Makefile's TEST_DATABASE_URL uses.
: "${TEST_DATABASE_URL:=$(sed -n 's/^DATABASE_URL=//p' .env 2>/dev/null)}"
if [ -z "${TEST_DATABASE_URL}" ]; then
	echo "perf-budget: TEST_DATABASE_URL is not set and .env has no DATABASE_URL" >&2
	exit 2
fi
export TEST_DATABASE_URL

log=$(mktemp)
trap 'rm -f "$log"' EXIT

# One go test invocation per package (sequential): a single combined run
# interleaves the packages' -v output, and identical shape names ("unfiltered
# page 1", …) would be indistinguishable in the report. Sequential also
# measures each fixture against a quiet database — the CI-like overlap is
# already exercised by `go test ./...`, whose retry protocol absorbs
# contention spikes.
packages=(
	./services/procurement/vendor
	./services/procurement/requisition
	./services/procurement/purchaseorder
	./services/procurement/invoice
	./services/procurement/dashboard
)

echo "perf-budget: running the 100k scale fixtures (5 packages, ~8 min)..." >&2
test_status=0
for pkg in "${packages[@]}"; do
	echo "### $pkg" >>"$log"
	echo "perf-budget: $pkg" >&2
	rc=0
	go test -count=1 -v -timeout 30m -run 'Scale100k' "$pkg" 2>&1 | tee -a "$log" || rc=$?
	if [ "$rc" -ne 0 ]; then
		test_status=1
		echo "perf-budget: $pkg — go test FAILED" >&2
	fi
done

# The tests log "<shape>: attempt N/M: median=X p95=Y over R runs (budget Bms)"
# per shape (the dashboard's shape is named "summary"). A shape that busts
# its budget retries (up to shapeAttempts=3); the LAST line per shape is the
# decisive one, so emit only the final occurrence keyed by package + shape.
# fmt_ms normalizes a Go duration (79.14ms, 1.2s, 531us) to milliseconds.
fmt_ms() {
	case "$1" in
	*ms) echo "${1%ms}" ;;
	*us) awk -v v="${1%us}" 'BEGIN {printf "%.1f", v/1000}' ;;
	*µs) awk -v v="${1%µs}" 'BEGIN {printf "%.1f", v/1000}' ;;
	*s) awk -v v="${1%s}" 'BEGIN {printf "%.1f", v*1000}' ;;
	*) echo "$1" ;;
	esac
}

rows=$(awk '
/^### / { pkg = $2; sub(/^services\/procurement\//, "", pkg) }
/ attempt [0-9]+\/[0-9]+: median=[^ ]+ p95=[^ ]+ over [0-9]+ runs \(budget [0-9]+ms\)/ {
	shape = $0
	sub(/: attempt [0-9]+\/[0-9]+: median=.*/, "", shape)
	sub(/^[ \t]*[^:]+:[0-9]+: /, "", shape) # go test -v prefixes "file:line: "
	stats = $0
	sub(/.* attempt [0-9]+\/[0-9]+: /, "", stats)
	last[pkg " " shape] = stats
	seen[pkg " " shape] = 1
	names[pkg " " shape] = pkg " " shape
}
END { for (s in seen) print names[s] "\t" last[s] }
' "$log" | sort)

echo
echo "perf-budget: per-shape p95 vs budget (final attempt per shape):"
fail=0
while IFS=$'\t' read -r shape rest; do
	[ -n "${shape:-}" ] || continue
	median=${rest#*median=}
	median=${median%% *}
	p95=${rest#*p95=}
	p95=${p95%% *}
	budget=${rest##*budget }
	budget=${budget%%ms*}
	p95_ms=$(fmt_ms "$p95")
	if awk -v p="$p95_ms" -v b="$budget" 'BEGIN {exit !(p < b)}'; then
		verdict=PASS
	else
		verdict=FAIL
		fail=1
	fi
	printf '  %-4s budget=%5sms p95=%9s median=%-11s %s\n' "$verdict" "$budget" "$p95" "$median" "$shape"
done <<<"$rows"

if [ "$test_status" -ne 0 ]; then
	fail=1
	echo "perf-budget: FAIL — go test exited non-zero" >&2
fi
if [ "$fail" -eq 0 ]; then
	echo "perf-budget: PASS — every shape is under budget (targets and protocol: docs/perf.md)"
fi
exit "$fail"
