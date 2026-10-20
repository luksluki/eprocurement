# Local onboarding

Goal: go from a fresh clone to a running, seeded local stack — without asking anyone a question. The README stays concise; this document carries the details.

Time budget: about 10 minutes on a typical laptop (the seed step dominates).

## 1. Prerequisites

| Tool | Version | Check | Notes |
|---|---|---|---|
| Git | any recent | `git --version` | |
| Go | ≥ 1.24 | `go version` | Matches `go.mod`. |
| Node.js | ≥ 22 | `node --version` | |
| pnpm | 12.8.1 | `pnpm --version` | Pinned via `packageManager` in `package.json`; enable with `corepack enable`. |
| Docker | Engine 27+ | `docker --version` | Docker Desktop, or Engine + Compose v2 plugin. |
| Docker Compose | v2 | `docker compose version` | v1 (`docker-compose`) is not supported. |
| GNU Make | 4+ | `make --version` | Windows: Git Bash and WSL2 both ship a working `make`. |

Everything runs through `make`; there is no per-service setup.

Windows note: use **Git Bash** (or WSL2) as your terminal so the Makefile targets and `.env` handling behave the same as on Linux/macOS.

## 2. Fresh clone walkthrough

Run these commands in order. Each step lists what success looks like.

```bash
git clone https://github.com/luksluki/eprocurement.git
cd eprocurement
```

### 2.1 Create `.env`

```bash
cp .env.example .env
```

`.env` is git-ignored and holds local-only credentials and ports. Docker Compose reads it automatically, and the seed tool loads it too. All defaults work out of the box — see [§3](#3-environment-variables) before changing anything.

### 2.2 Install JS dependencies

```bash
make install        # runs pnpm install
```

Success: `Done in ...s` with no `--frozen-lockfile` error.

### 2.3 Start the local stack

```bash
make compose-up     # docker compose up -d --wait
```

This starts PostgreSQL, MongoDB, Redis, RabbitMQ, MinIO, and the OTel collector, and **waits until every service is healthy**. First run pulls the images (~1–2 min); later runs take seconds.

```bash
make compose-ps     # all services should report "healthy"
```

| Service | Endpoint |
|---|---|
| PostgreSQL | `localhost:5432` |
| MongoDB | `localhost:27017` |
| Redis | `localhost:6379` |
| RabbitMQ (AMQP / UI) | `localhost:5672` / http://localhost:15672 (`eprocurement` / `eprocurement`) |
| MinIO (S3 / Console) | http://localhost:9000 / http://localhost:9001 (`eprocurement` / `eprocurement`) |
| OTel collector (gRPC / HTTP / health) | `localhost:4317` / `localhost:4318` / http://localhost:13133 |

### 2.4 Build

```bash
make build          # go build ./... + pnpm build
```

Success: no output from Go, and turbo reports all tasks successful.

### 2.5 Seed demo data

```bash
make seed
```

Wipes and regenerates the deterministic demo dataset (3 tenants, users, vendors, 100,000 requisitions, 1,000,000 domain events; fixed RNG seed 42). Completes in well under two minutes; on a typical laptop it is closer to 20 seconds.

Expected output (counts are deterministic):

```text
seeded tenants=3 users=15 vendors=300 requisitions=100000 purchase_orders=... events=1000000 in ...
```

Sanity-check the data:

```bash
docker compose exec postgres psql -U eprocurement -d eprocurement -c "select count(*) from demo.requisitions;"
docker compose exec mongo mongosh --quiet -u eprocurement -p eprocurement --authenticationDatabase admin --eval "db.getSiblingDB('eprocurement').events.countDocuments()"
```

### 2.6 Run the checks

```bash
make lint && make test
```

These are the same gates CI runs. They must pass before you push.

You now have a working environment. Head to [CONTRIBUTING.md](../CONTRIBUTING.md) for the Git Flow workflow, or [`AGENTS.md`](../AGENTS.md) if you are an AI coding agent.

## 3. Environment variables

`.env.example` documents every variable the stack consumes — copy it, never commit the copy. Compose interpolates `${VAR:-default}` from `.env`; the seed tool and services read connection strings from it directly (with the same defaults if the variable is absent).

**Ports** (published host ports for the compose stack):

| Variable | Default |
|---|---|
| `POSTGRES_PORT` | `5432` |
| `MONGO_PORT` | `27017` |
| `REDIS_PORT` | `6379` |
| `RABBITMQ_PORT` / `RABBITMQ_MANAGEMENT_PORT` | `5672` / `15672` |
| `MINIO_PORT` / `MINIO_CONSOLE_PORT` | `9000` / `9001` |
| `OTEL_GRPC_PORT` / `OTEL_HTTP_PORT` / `OTEL_HEALTH_PORT` | `4317` / `4318` / `13133` |

**Credentials** (local development only — all default to `eprocurement`): `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB`, `MONGO_ROOT_USER`, `MONGO_ROOT_PASSWORD`, `MONGO_DB`, `RABBITMQ_USER`, `RABBITMQ_PASSWORD`, `MINIO_ROOT_USER`, `MINIO_ROOT_PASSWORD`.

**Connection strings** (read by the seed tool and, later, by services):

| Variable | Default |
|---|---|
| `DATABASE_URL` | `postgres://eprocurement:eprocurement@localhost:5432/eprocurement?sslmode=disable` |
| `MONGO_URI` | `mongodb://eprocurement:eprocurement@localhost:27017/eprocurement?authSource=admin` |
| `REDIS_URL` | `redis://localhost:6379/0` |
| `RABBITMQ_URL` | `amqp://eprocurement:eprocurement@localhost:5672/` |
| `S3_ENDPOINT` | `http://localhost:9000` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://localhost:4317` |

**Application services** (optional; safe to omit): `APP_ENV` (default `local`) and `PORT` (default `8080`), read by `internal/platform/config`.

> **Port overrides have two halves.** Compose only maps ports — it does not rewrite connection strings. If you set `POSTGRES_PORT=55432`, you must also change the port inside `DATABASE_URL`, or `make seed` will keep dialing 5432. The same applies to `MONGO_PORT` + `MONGO_URI`, `RABBITMQ_PORT` + `RABBITMQ_URL`, `MINIO_PORT` + `S3_ENDPOINT`, and `OTEL_GRPC_PORT` + `OTEL_EXPORTER_OTLP_ENDPOINT`. After editing `.env`, recreate the stack: `make compose-down && make compose-up`.

## 4. Make targets

`make help` prints this list. Targets used daily:

| Target | What it does | When to use |
|---|---|---|
| `make install` | `pnpm install` | After cloning or when lockfile deps change. |
| `make build` | `go build ./...` + `pnpm build` | After pulling changes; also a CI gate. |
| `make lint` | `go vet ./...` + `pnpm lint` | Before every push; CI gate. |
| `make test` | `go test ./...` + `pnpm test` | Before every push; CI gate. |
| `make fmt` | `gofmt -w services internal scripts` | Before committing Go changes. |

Compose lifecycle:

| Target | What it does | When to use |
|---|---|---|
| `make compose-up` | Start stack, wait for health | Start of a work session. |
| `make compose-ps` | Show service health | Check the stack is up. |
| `make compose-logs` | Follow all stack logs | Debugging infrastructure. |
| `make compose-down` | Stop stack, **keep volumes** | Free resources; data survives. |

Data:

| Target | What it does | When to use |
|---|---|---|
| `make seed` | Wipe + regenerate deterministic demo data | Get back to a known dataset. |
| `make seed-reset` | Wipe demo data only, no seeding | Inspect the empty-schema state. |
| `make reset` | `compose down -v` + `compose up --wait` | Nuke volumes and start clean. |

See [§5](#5-demo-data-and-resets) for the difference between the reset levels.

## 5. Demo data and resets

Demo data lives in an isolated PostgreSQL `demo` schema and the MongoDB `events` collection — it never mixes with future application schemas. `make seed` always resets first, so reruns are reproducible (identical UUIDs, names, amounts, and event order — RNG seed 42).

Three reset levels, from gentle to nuclear:

| Command | PostgreSQL | MongoDB | Volumes | Use when |
|---|---|---|---|---|
| `make seed-reset` | Truncates `demo` schema | Drops `events` collection | kept | You want an empty demo state. |
| `make seed` | `seed-reset` + regenerate | same + regenerate | kept | You want the standard dataset back. |
| `make reset` | Wipes the whole cluster | Wipes everything | **dropped** | Corrupt state, changed credentials, or a clean slate. |

Lighter seed (faster on slow machines; any counts work, output stays deterministic per `--seed`):

```bash
go run ./scripts/seed --tenants 2 --requisitions 10000 --events 100000
```

All flags: `--dsn`, `--mongo-uri`, `--mongo-db`, `--tenants`, `--vendors-per-tenant`, `--requisitions`, `--events`, `--seed`, `--reset`.

## 6. Troubleshooting

**`make: command not found` (Windows)** — run the walkthrough from Git Bash or WSL2, not PowerShell/cmd. Git Bash ships with `make`; verify with `make --version`.

**`docker: Cannot connect to the Docker daemon`** — Docker Desktop is not running. Start it, wait for `docker info` to succeed, then retry `make compose-up`.

**Port already in use (`bind: address already in use`)** — another process owns the default port (a native PostgreSQL on 5432 is the classic case). Override the port in `.env` and update the matching connection string (see the note in [§3](#3-environment-variables)); for example `POSTGRES_PORT=55432` plus `DATABASE_URL=postgres://...@localhost:55432/...`. Then `make compose-down && make compose-up`. On Windows, `netstat -ano | findstr :5432` finds the culprit.

**`compose up --wait` times out / a service stays `unhealthy`** — check where it is stuck: `make compose-ps`, then `make compose-logs`. Most often the image pull was slow; rerunning `make compose-up` resumes. If a container restart-loops, `docker compose logs <service>` shows why.

**`seed: postgres ping` or `mongo connect` fails** — either the stack is not healthy yet (`make compose-ps`), or you changed a port in `.env` without updating `DATABASE_URL` / `MONGO_URI` (the two-halves rule in [§3](#3-environment-variables)). The seed tool connects from the host, so it uses `localhost` + the published ports, not the container-internal ones.

**`pnpm install` fails with a lockfile mismatch** — do not hand-edit `pnpm-lock.yaml`. Run `pnpm install` (no `--frozen-lockfile`) to regenerate, and commit the updated lockfile with your dependency change. If you changed nothing, `git pull` first — your lockfile is stale.

**Seed feels slow or the machine is constrained** — use the lighter flags from [§5](#5-demo-data-and-resets), or skip seeding entirely until you need data. Docker Desktop on 8 GB machines: give the VM at least 6 GB in *Settings → Resources*.

**Weird state after experiments (wrong credentials, half-migrated data)** — `make reset` drops all volumes and recreates the stack from your current `.env`, then `make seed` restores the standard dataset.

**`git status` shows `.env` as modified/untracked** — it should always be untracked and ignored. If you accidentally committed one, rotate the credentials it contained — treat them as leaked (even though they are local defaults).

## 7. Where to go next

- [CONTRIBUTING.md](../CONTRIBUTING.md) — Git Flow, PR checklist, Definition of Done
- [`AGENTS.md`](../AGENTS.md) — the same workflow, written for AI coding agents
- [ADR index](adr/README.md) — architecture decisions behind the stack
- [PRD v0.2](prd/prd-v0.2.md) — what this platform is building
- [Backlog map](backlog-v0.1.md) — milestones M0–M5
