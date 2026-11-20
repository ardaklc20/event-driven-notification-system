# Slice 1 — Walking skeleton

[Delivery plan](../PLAN.md) · Branch `feat/skeleton` · Core, plus [B6] by [K29] · Depends on: nothing

## Goal

On a clean clone, `docker compose up` starts PostgreSQL, applies the migrations and runs an API that reports healthy. From this pull request on, every pull request runs lint, the tests with the race detector, the OpenAPI check and the image build in CI.

## What this slice delivers

| ID | Delivered here |
|---|---|
| [R16] | `GET /healthz` (liveness, no dependency) and `GET /readyz` (database ping, `503` during shutdown) ([K30]) |
| [D3] | `docker compose up` brings up `postgres`, `migrate` and `api`; the worker and the mock join in slice 3 |
| [D4] | Huma generates the OpenAPI 3.1 document; `api/openapi.yaml` is committed and checked for drift ([K19]) |
| [D5] | goose migrations embedded in the binary and run by the one-off `migrate` service ([K18]) |
| [D6] | `go test -count=1 ./...` with Testcontainers ([K22]) |
| [B6] | GitHub Actions workflow ([K29]) |

## Preconditions

- The first migration and `docker-compose.yml` follow [K36] (UUIDv7 IDs), [K37] (enum types for priority and status), [K38] (`postgres:18.7-trixie`) and [K39] (configurable host ports bound to `127.0.0.1`).
- golangci-lint v2.15.0 is installed locally ([K28]).

## Files

| Path | Purpose |
|---|---|
| `go.mod`, `go.sum` | Module, Go version of the build image ([K31]) |
| `cmd/api/` | Entry point: configuration, logger, pool, HTTP server, signal handling; subcommands `healthcheck` and `openapi` |
| `cmd/migrate/` | Entry point: `migrate up` through a goose Provider ([K18]) |
| `internal/config/` | API configuration struct, `Validate()` and table tests ([K32]) |
| `internal/httpapi/` | `ServeMux`, Huma API, health operations, readiness state, tests ([K16], [K19], [K30]) |
| `internal/postgres/` | `pgxpool` setup ([K17]) |
| `internal/testdb/` | Test helper: one PostgreSQL container per package, one migrated database per test ([K22]) |
| `migrations/embed.go`, `migrations/00001_create_notifications.sql` | Embedded migrations ([K18]) |
| `api/openapi.yaml` | Generated and committed document ([K19]) |
| `Dockerfile`, `.dockerignore` | Multi-stage build of `api` and `migrate` only ([K31]) |
| `docker-compose.yml` | `postgres`, `migrate`, `api` ([K1], [K6]) |
| `.golangci.yml` | Linter configuration ([K28]) |
| `.github/workflows/ci.yml` | CI ([K29]) |
| `scripts/wait-health.sh` | Bounded wait for a container's health state, used by every stack check |
| `README.md` | Setup and test command ([D2]) |

## Schema: `00001_create_notifications`

The table is the queue ([K2]). Columns added by later slices are listed in their own notes. The migration first creates the two enum types of [K37]: `notification_priority AS ENUM ('high', 'normal', 'low')` and `notification_status AS ENUM ('queued', 'processing', 'sent', 'failed', 'cancelled')`; its `Down` section drops the table, then the types.

| Column | Type | Rule |
|---|---|---|
| `id` | `uuid` | Primary key, default `uuidv7()` ([K36]) |
| `recipient` | `text` | Not null |
| `channel` | `text` | Not null; `sms`, `email` or `push` (check constraint) |
| `content` | `text` | Not null |
| `priority` | `notification_priority` | Not null; the declared order is `high → normal → low`, so `ORDER BY priority` is the explicit order [K8] needs ([K37]) |
| `status` | `notification_status` | Not null, default `queued`; one of the five statuses ([K9], [K37]) |
| `attempts` | `integer` | Not null, default 0, at least 0 |
| `available_at` | `timestamptz` | Not null, default the insert time |
| `lease_token` | `uuid` | Null outside `processing` |
| `lease_expires_at` | `timestamptz` | Null outside `processing` |
| `last_error` | `text` | Null |
| `created_at`, `updated_at` | `timestamptz` | Not null |

A check constraint ties the lease to the status: the two lease columns are both set exactly when `status` is `processing`. This makes [K3]'s rule that every transition out of `processing` clears the lease fields a database rule, not only an application rule.

## Steps

1. **Module.** Create the module and the directories this slice needs, and nothing more ([K23]).
2. **Configuration.** API struct read with `env.Parse`: `DATABASE_URL` (`required`, `notEmpty`, no default), `HTTP_ADDR` (default `:8080`), `LOG_LEVEL` (default `info`), `SHUTDOWN_TIMEOUT` (default `10s`). `Validate()` checks what tags cannot. Test: `TestAPIConfig`.
3. **Migrations.** `migrations/embed.go` embeds `*.sql`; the first file creates the table above. `cmd/migrate up` builds a goose Provider over pgx's `stdlib` connection, runs `Up(ctx)`, logs each applied version as JSON, and exits with 1 on any error. Test: `TestMigrations`.
4. **Test helper.** `internal/testdb` starts `postgres:18.7-trixie`, the tag Compose names ([K22], [K38]), once per package in `TestMain`, creates a database per test, applies the embedded migrations and drops it in `t.Cleanup`. Tests: `TestDatabaseIsolation`, `TestTestDBImageMatchesCompose`.
5. **Pool.** `internal/postgres` opens `pgxpool` and exposes a ping with a short time limit for readiness.
6. **HTTP layer.** `ServeMux` with Huma through `humago`; operations `GET /healthz` and `GET /readyz`. Readiness returns `200` with each check's result, or `503` naming the failing check; it returns `503` once shutdown has started. Health requests are logged at debug level ([K21]). Tests: `TestRouter`, `TestHealthEndpoints`, `TestReadyzDuringShutdown`.
7. **API entry point.** `cmd/api` wires the parts. On `SIGTERM` it marks itself not ready, calls `http.Server.Shutdown` with `SHUTDOWN_TIMEOUT`, closes the pool and exits with 0.
8. **`healthcheck` subcommand.** `api healthcheck` calls `http://127.0.0.1<port>/readyz` with a 2-second limit and exits with 0 on `200`, otherwise 1. It reads only the listen address ([K31]). Test: `TestHealthcheckCommand`.
9. **OpenAPI.** `api openapi` prints the generated document. `api/openapi.yaml` is that output, committed. Test: `TestOpenAPIUpToDate` generates the document in memory and compares it with the committed file.
10. **Image.** Build stage `golang:1.27.4-trixie`: `go mod download` in its own layer, then `CGO_ENABLED=0` builds of `./cmd/api` and `./cmd/migrate`. Runtime stage `gcr.io/distroless/static-debian13:nonroot` with the two binaries under `/app/`, no `ENTRYPOINT` ([K31]).
11. **Compose.** `postgres` (image `postgres:18.7-trixie`, port `${BIND_ADDRESS:-127.0.0.1}:${POSTGRES_PORT:-5433}:5432`, `pg_isready` health check, a named volume mounted at `/var/lib/postgresql`, where the image declares its volume from version 18 on ([K38])), `migrate` (`command: ["/app/migrate", "up"]`, after `postgres` is healthy), `api` (`command: ["/app/api"]`, port `${BIND_ADDRESS:-127.0.0.1}:${API_PORT:-8080}:8080`, after `postgres` is healthy and `migrate` completed successfully, health check `["CMD", "/app/api", "healthcheck"]`, `stop_grace_period: 15s`) ([K6], [K39]).
12. **Lint.** `.golangci.yml`: `version: "2"`, `linters.default: standard`, `bodyclose` and `noctx` enabled, `goimports` as formatter ([K28]).
13. **CI.** On every pull request and every push to `main`. Every job checks out with `actions/checkout` and `ref: ${{ github.event.pull_request.head.sha || github.sha }}`, so a pull request run tests the pull request's head commit rather than GitHub's default merge commit, the same commit the release check clones ([slice 10](10-core-release-check.md#release-check)), and prints `tested-sha=$(git rev-parse HEAD)` right after the checkout. Then: `actions/setup-go` v7 with the version from `go.mod`; golangci-lint through its v9 action, pinned to v2.15.0; `go test -count=1 -race ./...`; the OpenAPI check (`go run ./cmd/api openapi` compared with `api/openapi.yaml`, failing on any difference); `docker build .` ([K29]).
14. **Wait script.** `scripts/wait-health.sh [-p project] <service> <state> <tries>` reads the service container's health every 2 seconds. It prints `<service>=<state>` and exits with 0 when the wanted state is reached, and prints the last state seen and exits with 1 after the last try. It runs as its own process, so a failed wait fails the check without closing the caller's shell. Every stack check in this plan waits through it.
15. **README.** Requirements (Docker running), `docker compose up`, the host ports (`API_PORT` 8080, `POSTGRES_PORT` 5433) bound to `127.0.0.1` unless `BIND_ADDRESS` is set ([K39]), and `go test -count=1 ./...`. The README's commands use the default ports.

## Tests

| Test | Package | Kind | Setup | Asserts |
|---|---|---|---|---|
| `TestAPIConfig` | `internal/config` | Unit | Variables given through `Options.Environment` | Defaults applied; a missing and an empty `DATABASE_URL` both rejected; several errors in one result |
| `TestMigrations` | `migrations` | Database | Fresh database | First run creates the two enum types, the table and its constraints; a second run applies nothing. A row inserted without `id` gets a UUID whose `uuid_extract_version` is 7 ([K36]); an insert with `priority = 'urgent'` or `status = 'pending'` is rejected by the database ([K37]) |
| `TestLeaseMatchesStatus` | `migrations` | Database | Fresh database; one insert per combination of status (`processing`, `queued`) and lease columns (both set, only `lease_token`, only `lease_expires_at`, neither) | Accepted: `processing` with both, `queued` with neither. The other six combinations are rejected by the check constraint |
| `TestDatabaseIsolation` | `internal/testdb` | Database | Two tests in one package | A row written in one test is not visible in the other |
| `TestTestDBImageMatchesCompose` | `internal/testdb` | Unit | Reads `docker-compose.yml` as text from the repository root | The `postgres` service's `image:` line names exactly the helper's tag, `postgres:18.7-trixie` ([K22], [K38]) |
| `TestRouter` | `internal/httpapi` | HTTP | Real router | Unknown path `404`; known path with the wrong method `405` with an `Allow` header |
| `TestHealthEndpoints` | `internal/httpapi` | HTTP + database | Subtests: database reachable; database refusing connections | Reachable: both `200`. Unreachable: `/healthz` `200`, `/readyz` `503` naming the database check |
| `TestReadyzDuringShutdown` | `internal/httpapi` | HTTP | Readiness state switched to shutting down | `/readyz` returns `503` |
| `TestHealthcheckCommand` | `internal/httpapi` | HTTP | `httptest` server answering `200`, then `503`, then nothing | Exit status 0, 1, 1 |
| `TestOpenAPIUpToDate` | `internal/httpapi` | Unit | Generated document | Equal to `api/openapi.yaml`, byte for byte |

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Configuration | `go test -count=1 -v -run '^TestAPIConfig$' ./internal/config/` | `--- PASS: TestAPIConfig` |
| Migrations ([D5]) | `go test -count=1 -v -run '^(TestMigrations\|TestLeaseMatchesStatus)$' ./migrations/` | Both `--- PASS` lines |
| Test isolation and image ([K22], [K38]) | `go test -count=1 -v -run '^(TestDatabaseIsolation\|TestTestDBImageMatchesCompose)$' ./internal/testdb/` | Both `--- PASS` lines |
| Router ([K16]) | `go test -count=1 -v -run '^TestRouter$' ./internal/httpapi/` | `--- PASS: TestRouter` |
| Health ([R16]) | `go test -count=1 -v -run '^(TestHealthEndpoints\|TestReadyzDuringShutdown\|TestHealthcheckCommand)$' ./internal/httpapi/` | Three `--- PASS` lines |
| OpenAPI ([D4]) | `go test -count=1 -v -run '^TestOpenAPIUpToDate$' ./internal/httpapi/` | `--- PASS: TestOpenAPIUpToDate` |
| No cached results ([K22]) | `go test -count=1 ./...` twice | The second run shows durations and no `(cached)` |
| Stack starts ([D3]) | [Stack check](#stack-check) | `postgres` healthy, `migrate` exited with 0, `api` healthy, within 120 seconds |
| Local ports ([K39]) | `docker compose ps --format '{{.Service}} {{.Ports}}'` | `postgres` shows `127.0.0.1:5433->5432/tcp` and `api` shows `127.0.0.1:8080->8080/tcp`; no port is published on `0.0.0.0` or `[::]` |
| PostgreSQL version ([K38]) | `docker compose exec postgres postgres --version` | A line beginning with `postgres (PostgreSQL) 18.7` |
| Second migration run ([D5]) | `docker compose run --rm migrate` | Exit code 0; the log reports no migration applied |
| Non-root image ([K31]) | `docker inspect --format '{{.Config.User}}' "$(docker compose images -q api)"` | A non-root user (`nonroot` or `65532`); not empty, not `root` |
| Image contents ([K31]) | `docker run --rm --entrypoint /app/api "$(docker compose images -q api)" healthcheck; echo "exit=$?"` | `exit=1` (nothing listens inside a lone container), proving the binary runs without a shell |
| CI ([B6]) | `gh pr checks`, then `gh run view <run> --log \| grep tested-sha` for the run on this pull request | `lint`, `test`, `openapi` and `docker` jobs show `pass` on this pull request; `tested-sha` equals the pull request's head SHA (`gh pr view --json headRefOid`) |

## Stack check

```bash
docker compose up -d --build
scripts/wait-health.sh api healthy 60
docker compose ps -a --format 'table {{.Service}}\t{{.State}}\t{{.Health}}\t{{.ExitCode}}'
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/healthz
curl -s http://localhost:8080/readyz
```

**Expected observation:** the wait prints `api=healthy` and exits with 0; the table shows `postgres running healthy`, `migrate exited 0` and `api running healthy`; `/healthz` prints `200`; `/readyz` returns `200` with the database check reported as passing. If the wait exits with 1, the check fails.

## Failure scenarios

### Database unreachable

- **Setup:** The stack from the stack check is running.
- **Command:** `docker compose stop postgres`, then `scripts/wait-health.sh api unhealthy 60`, then `curl -s -w '\n%{http_code}\n' http://localhost:8080/readyz`; afterwards `docker compose start postgres` and `scripts/wait-health.sh api healthy 60`.
- **Expected observation:** The API container stays `running` and the first wait exits with 0 (`api=unhealthy`); Docker does not restart it ([K30]). `/readyz` answers `503` and names the database check; `/healthz` still answers `200`. After PostgreSQL starts again, the second wait exits with 0 (`api=healthy`).

### Broken migration

This check deletes its own volume, so it runs under a separate Compose project and never touches the main stack's data.

- **Setup:** Stop the main stack (`docker compose stop`; no `down -v`). Add an uncommitted file `migrations/99999_broken.sql` whose `-- +goose Up` section contains invalid SQL.
- **Command:**

  ```bash
  P="nsys-migration-check-$(date +%s)"
  docker compose -p "$P" up -d --build    # expected to report that migrate failed
  for i in $(seq 1 30); do
    STATE=$(docker compose -p "$P" ps -a --format '{{.State}}' migrate)
    [ "$STATE" = exited ] && break
    sleep 2
  done
  echo "migrate=$STATE"
  docker compose -p "$P" ps -a --format 'table {{.Service}}\t{{.State}}\t{{.ExitCode}}'
  docker compose -p "$P" logs migrate
  ```

- **Expected observation:** `migrate=exited` within the loop's 60 seconds (otherwise the check fails); the table shows `migrate` with exit code 1, and its log names `99999_broken.sql`. `api` is not `running` (Compose leaves it `created` or does not create it). The migrations before the broken file stay applied ([K18]).
- **Clean-up:** `docker compose -p "$P" down -v` (only this run's project), delete the broken file, then `docker compose start` for the main stack.

### Shutdown

- **Setup:** The stack is running and healthy.
- **Command:** `docker compose stop api`, then `docker compose ps -a --format 'table {{.Service}}\t{{.State}}\t{{.ExitCode}}'` and `docker compose logs api | tail -5`.
- **Expected observation:** `api` shows `exited 0` (a graceful exit, not a kill after `stop_grace_period`), and the last log lines show readiness turning off before the server stopped.

## Evidence for the pull request

- Output of every acceptance check, including the `docker compose ps` table and the CI job list.
- Image size from `docker image ls`, for later comparison.

## Documentation

- `README.md`: setup, the test command, the database port.
- `AI_USAGE.md`: slice log row; evidence rows for [R16], [D3], [D4], [D5], [D6] and [B6] with the commands actually run.

[R16]: ../REQUIREMENTS.md#observability
[B6]: ../REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[D2]: ../REQUIREMENTS.md#deliverables
[D3]: ../REQUIREMENTS.md#deliverables
[D4]: ../REQUIREMENTS.md#deliverables
[D5]: ../REQUIREMENTS.md#deliverables
[D6]: ../REQUIREMENTS.md#deliverables
[K1]: ../DECISIONS.md#k1--database-postgresql
[K2]: ../DECISIONS.md#k2--queue-the-notifications-table-in-postgresql-is-the-queue
[K3]: ../DECISIONS.md#k3--delivery-guarantee-a-durable-queue-lease-recovery-and-bounded-retries
[K6]: ../DECISIONS.md#k6--process-layout-the-api-and-the-worker-run-as-separate-processes-from-one-go-module
[K8]: ../DECISIONS.md#k8--priority-strict-order-within-each-channel
[K9]: ../DECISIONS.md#k9--status-model-and-cancellation-five-statuses-and-pending-means-queued
[K16]: ../DECISIONS.md#k16--http-router-the-standard-librarys-nethttp
[K17]: ../DECISIONS.md#k17--database-access-pgx-with-plain-sql-in-the-repository
[K18]: ../DECISIONS.md#k18--migrations-goose-with-sql-files-embedded-in-the-binary
[K19]: ../DECISIONS.md#k19--openapi-generated-from-the-code-with-huma-on-the-standard-library-router
[K21]: ../DECISIONS.md#k21--logging-logslog-json-with-a-correlation-id-stored-on-each-notification
[K22]: ../DECISIONS.md#k22--test-database-testcontainers-with-the-same-postgresql-image-as-compose
[K23]: ../DECISIONS.md#k23--project-layout-small-packages-by-layer-with-multi-table-transactions-inside-postgres
[K28]: ../DECISIONS.md#k28--linter-golangci-lints-standard-set-plus-bodyclose-and-noctx
[K29]: ../DECISIONS.md#k29--ci-b6-a-github-actions-pipeline-for-lint-tests-the-openapi-check-and-the-docker-build
[K30]: ../DECISIONS.md#k30--health-checks-r16-healthz-for-liveness-readyz-for-readiness
[K31]: ../DECISIONS.md#k31--image-d3-a-multi-stage-build-on-a-distroless-non-root-image
[K32]: ../DECISIONS.md#k32--configuration-typed-environment-variables-with-caarlos0env
[K36]: ../DECISIONS.md#k36--notification-ids-uuidv7-generated-by-postgresql
[K37]: ../DECISIONS.md#k37--priority-and-status-two-postgresql-enum-types
[K38]: ../DECISIONS.md#k38--postgresql-image-postgres187-trixie-pinned
[K39]: ../DECISIONS.md#k39--host-ports-configurable-bound-to-127001
