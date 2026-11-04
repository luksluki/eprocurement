# ADR-0003 — Demo hosting: local-first with tunnel and video

- **Status:** Accepted
- **Date:** 2026-10-20
- **Related:** PRD §25 (D5), hosting addendum

## Context

The shared ELCODE VPS (3.6 GB RAM, ≈1.6 GB available, 9.4 GB free disk, 2 vCPU, 11 production containers, swap already in use) cannot host the full ProcureKit stack safely as an always-on service. The platform needs a reliable way to run demos without permanent hosting.

## Decision

Adopt **Option A**:

1. **Primary artifact:** a 5-minute demo video (English subtitles) recorded locally.
2. **Live demos:** run the stack locally (`docker compose --profile demo`) and expose it through a **Cloudflare Tunnel** on demand.
3. **No always-on public deployment** in Phase 0.
4. Revisit hosting only if a specific opportunity requires a permanent public URL; then upgrade the VPS (8 GB RAM, larger disk) or use a separate demo host.

## Alternatives considered

| Alternative | Why rejected |
|---|---|
| Always-on on the ELCODE VPS | Insufficient RAM/disk; risks production services (OOM, disk pressure) |
| On-demand trimmed profile on the VPS | Possible but tight; only if a demo must originate from a public host |
| Managed Kubernetes | Cost and ops overhead far exceed the value for a demo environment |
| Fly.io/Railway | Adds external dependencies and cost; local tunnel achieves the same demo result |

## Consequences

**Positive:** zero hosting cost; no risk to production; full observability stack can run during demos; easy reset via seed scripts.

**Negative:** live demo depends on the presenter's laptop and network; no permanent public URL.

**Mitigations:** pre-recorded video; tunnel runbook; `make demo` script; seeded reset; memory-capped compose profile.
