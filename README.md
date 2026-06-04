# workspace

Manages the contract and collaboration domain for the CoverOnes platform — contract lifecycle, e-signatures, task tracking, and work logs between clients and freelancers.

## What it does

- Receives contracts created automatically when a bid is accepted in marketplace (via S2S)
- Tracks contract state through draft → pending signature → active → completed / cancelled
- Collects e-signatures from both parties before a contract becomes active
- Provides task management within a contract (milestones / deliverables)
- Provides work log entries for time/effort tracking against a contract
- KYC tier gates: contract reads require Tier 1; writes (sign, update, tasks, worklogs) require Tier 2

## Where it sits

workspace sits behind the API gateway and accepts both gateway-routed user requests and internal S2S calls from marketplace. Clients do not create contracts directly — they are always created by marketplace after a bid is accepted.

```
browser → gateway → workspace
marketplace (S2S, AcceptBid) → workspace /internal/v1/contracts
```

## API (high level)

All user-facing routes are under `/v1`. An internal S2S endpoint is under `/internal/v1` and is not exposed via the gateway. Full request/response contract: [`conventions/http-api.md`](../conventions/http-api.md).

| Group | Endpoints | Min tier |
|-------|-----------|----------|
| Health | `GET /healthz`, `GET /readyz` | public |
| Contracts (read) | `GET /contracts`, `GET /contracts/:id` | 1 |
| Contracts (write) | `PATCH /contracts/:id`, `POST /contracts/:id/submit-for-signature`, `POST /contracts/:id/sign`, `POST /contracts/:id/complete`, `POST /contracts/:id/cancel` | 2 |
| Signatures | `GET /contracts/:id/signatures` | 1 |
| Tasks | `GET /contracts/:id/tasks` | 1 |
| Tasks (write) | `POST /contracts/:id/tasks`, `PATCH /contracts/:id/tasks/:taskId`, `DELETE /contracts/:id/tasks/:taskId` | 2 |
| Worklogs | `GET /contracts/:id/worklogs` | 1 |
| Worklogs (write) | `POST /contracts/:id/worklogs`, `DELETE /contracts/:id/worklogs/:worklogId` | 2 |
| S2S (internal) | `POST /internal/v1/contracts` | service token |

## Tech

| Item | Detail |
|------|--------|
| Language | Go 1.25 |
| Framework | Gin |
| Database | PostgreSQL (pgx v5) |
| Cache / events | Redis (optional; falls back gracefully) |
| Migrations | golang-migrate, embedded SQL |

## Project structure

```
cmd/server/      — entrypoint; wires config, pool, services, router
internal/
  config/        — env-based config loading
  domain/        — core types (Contract, Signature, Task, Worklog)
  service/       — business logic
  store/postgres — SQL queries and transaction manager
  handler/       — HTTP handlers and router (user-facing + internal)
  platform/      — shared middleware, health, logger
  events/        — Redis publisher and noop fallback
migrations/      — versioned SQL migration files
```

## Run locally

```sh
cd ../dev-stack && docker compose up -d
```

See [`dev-stack/README.md`](../dev-stack/README.md) for full setup instructions.

## Environment variables

| Variable | Purpose |
|----------|---------|
| `WORKSPACE_PORT` | HTTP listen port (default 8082) |
| `WORKSPACE_POSTGRES_DSN` | PostgreSQL connection string |
| `WORKSPACE_DB_SCHEMA` | Postgres schema for multi-tenant DB sharing |
| `WORKSPACE_REDIS_URL` | Redis URL (optional) |
| `WORKSPACE_CONTRACT_SERVICE_TOKEN` | Token required by marketplace for S2S contract creation |
| `WORKSPACE_GATEWAY_HMAC_SECRET` | Shared secret for verifying gateway-origin requests |
| `WORKSPACE_AUTO_MIGRATE` | Set `true` to apply migrations on startup (dev/CI only) |
| `WORKSPACE_LOG_LEVEL` | Log level (debug / info / warn / error) |
