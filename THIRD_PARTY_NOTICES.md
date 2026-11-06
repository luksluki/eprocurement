# Third-party notices

ELCODE eProcurement is licensed under the Apache License 2.0 (see `LICENSE`).
Third-party components keep their own licenses. This file lists the direct
dependencies and how to regenerate the full inventory.

## Go modules

Defined in `go.mod`. Direct requirements:

| Module | Version |
|---|---|
| github.com/gofiber/fiber/v2 | v2.52.6 |
| github.com/google/uuid | v1.6.0 |
| github.com/jackc/pgx/v5 | v5.7.4 |
| github.com/minio/minio-go/v7 | v7.0.83 |
| github.com/prometheus/client_golang | v1.23.0 |
| github.com/prometheus/client_model | v0.6.2 |
| github.com/rabbitmq/amqp091-go | v1.16.0 |
| github.com/redis/go-redis/v9 | v9.14.1 |
| go.mongodb.org/mongo-driver | v1.17.2 |
| go.opentelemetry.io/otel | v1.39.0 |
| go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc | v1.39.0 |
| go.opentelemetry.io/otel/sdk | v1.39.0 |
| go.opentelemetry.io/otel/trace | v1.39.0 |
| golang.org/x/crypto | v0.47.0 |
| google.golang.org/grpc | v1.80.0 |
| google.golang.org/protobuf | v1.36.11 |

Most of these are Apache-2.0, MIT, or BSD-3-Clause. The authoritative license
text for each module ships inside the module cache after `go mod download`.

## npm packages

Defined in the workspace `package.json` files.

| Package | Version | Where |
|---|---|---|
| next | ^15.5.9 | apps/web |
| react | ^19.3.1 | apps/web |
| react-dom | ^19.3.1 | apps/web |
| @tanstack/react-query | ^5.104.5 | apps/web |
| @tanstack/react-table | ^8.21.3 | apps/web |
| react-hook-form | ^7.91.0 | apps/web |
| @hookform/resolvers | ^6.0.0 | apps/web |
| zod | ^4.6.7 | apps/web |

Most of these are MIT. License texts are available in `node_modules` after
`pnpm install`.

## Regenerate the full inventory

```bash
go list -m all
pnpm licenses list --prod
```

## Notes

- Demo data is synthetic. It contains no client records, personal data, or
  production exports.
- Container images referenced by `compose.yaml` keep their upstream licenses.
- If a dependency is added that carries a copyleft license, record it here
  before merging.
