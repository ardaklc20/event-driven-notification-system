# Slice 3 — Delivery to the provider

[Delivery plan](../PLAN.md) · Branch `feat/delivery` · Core · Depends on: slice 2

## Goal

A separate worker process claims queued notifications under a lease and sends them to the provider. In Compose, a created notification reaches `sent` through the mock provider, and the API answers while the provider is still busy.

## What this slice delivers

| ID | Delivered here |
|---|---|
| [R7] | The API never calls the provider; the worker does, asynchronously |
| [R11] | One valid lease at a time ([K2], [K3]); the replay test after `sent` ([K10]) |
| [R12] | Calls with a 5-second timeout and a deadline inside the lease; no redirects ([K14]) |
| [R13] | `POST {to, channel, content}`; `202` with `{messageId, status, timestamp}` stored ([K15]) |
| [R15] | Correlation ID from the row to the worker's log lines and the provider's `X-Correlation-ID` header ([K21]) |
| [N2] | Several workers can run against the same table |
| [A2] | One `PROVIDER_URL` for every channel; the channel travels in the body |

## Preconditions

- Slice 2 merged.

## Files

| Path | Purpose |
|---|---|
| `cmd/worker/` | Entry point: configuration, logger, pool, claim loop, signal handling |
| `cmd/mockprovider/` | Entry point for the mock |
| `internal/worker/` | Claim loop, in-flight limit, attempt logging, result handling ([K2], [K21]) |
| `internal/provider/` | HTTP client: request body, headers, timeout, no redirects, `202` decoding ([K14], [K15]) |
| `internal/mockprovider/` | Mock handlers, arrival records, `/stats` ([K14]) |
| `internal/postgres/` | Claim and result-write statements ([K3], [K9]) |
| `internal/config/` | Worker struct and `Validate()` ([K32]) |
| `internal/e2e/` | Test-only package that runs the API handler, the worker and an `httptest` provider against one test database |
| `migrations/00003_provider_result.sql` | Schema below |
| `Dockerfile`, `docker-compose.yml` | The image now builds all four programs ([K31]); `worker` and `mockprovider` services |

## Schema: `00003_provider_result`

| Column | Type | Rule |
|---|---|---|
| `provider_message_id` | `text` | Null; no unique constraint ([K15]) |
| `provider_timestamp` | `timestamptz` | Null |
| `sent_at` | `timestamptz` | Null; set with `sent` |

## The claim, in this slice

One transaction ([K2], [K3], [K9]):

1. Select one row with `status = 'queued'` and `available_at <= clock_timestamp()`, ordered by `available_at, id`, `FOR UPDATE SKIP LOCKED`. (Priority order and the permit arrive in slice 5.)
2. Set `status = 'processing'`, a new `lease_token`, `lease_expires_at = clock_timestamp() + lease duration`, `attempts = attempts + 1`.
3. Return the row and the **remaining lease as a duration** computed by the database.

The worker reads its monotonic clock **just before it sends the claim** (`t0`) and sets its local deadline to `t0 + remaining`. `t0` comes before the moment the database computed the remaining time, so the local deadline can only fall earlier than the database's lease end, never later, whatever the clock difference or network delay. Adding the duration when the answer arrives would extend the lease by the claim's round trip.

## The result write

Every result is one statement conditioned on `status = 'processing'`, the claim's `lease_token` and `lease_expires_at > clock_timestamp()` ([K3], [K9]). It clears `lease_token` and `lease_expires_at` in the same statement. If it changes no row, the worker logs that its lease was lost and does nothing else.

| Provider outcome | Write in this slice |
|---|---|
| `202`, body valid | `sent`, `sent_at`, `provider_message_id`, `provider_timestamp` |
| `202`, body broken or unreadable before the deadline | `sent`, `sent_at`, provider fields empty, a warning line ([K15]) |
| Anything else (including `3xx`, which is not followed) | `failed` with `last_error`. **Interim:** slice 4 replaces this with [K7]'s classification |

When the HTTP call's context has ended, the result is written with a **new** database context whose deadline is the remaining lease ([K15]), never with the expired one.

## Worker settings ([K32])

| Variable (proposal) | Default | Rule |
|---|---|---|
| `DATABASE_URL` | — | `required`, `notEmpty` |
| `PROVIDER_URL` | — | `required`, `notEmpty` |
| `PROVIDER_PROFILE` | `standard` | `standard` or `webhook_site_free` ([K14]) |
| `HTTP_TIMEOUT` | `5s` | ([K7]) |
| `RESULT_BUDGET` | `5s` | Time kept for writing a result |
| `LEASE_DURATION` | `30s` | `Validate()`: at least `HTTP_TIMEOUT + RESULT_BUDGET` |
| `IN_FLIGHT` | `20` | Calls in flight per worker; measured in slice 5 ([K5]) |
| `IDLE_WAIT` | `200ms` | Wait when the queue is empty ([K2]); from slice 5 on, only after an `empty` claim, never after a permit that is not yet due |
| `SHUTDOWN_BUDGET` | `10s` | ([K6]) |
| `LOG_LEVEL` | `info` | ([K21]) |

## Steps

1. **Migration** `00003`; extend `TestMigrations`.
2. **Claim and result statements** in `internal/postgres`, with `TestClaimSetsLease`, `TestSingleLease`, `TestResultRequiresValidLease` and `TestDuplicateMessageID`.
3. **Provider client.** JSON body `{to, channel, content}` with `to` taken from `recipient`; headers `Content-Type: application/json` and `X-Correlation-ID`; a client whose redirect policy returns the first response unchanged; the request context ends at the earlier of `HTTP_TIMEOUT` and the lease deadline minus `RESULT_BUDGET`; `202` body decoded into `messageId`, `status`, `timestamp`. Tests: `TestNoRedirect`.
4. **Worker loop.** Claim while work is found and a call slot is free; wait `IDLE_WAIT` when nothing is found; the lease deadline from `t0` as above. Each worker process creates a `worker_id` at start (host name plus a random suffix) and puts it on every log line. For each attempt: a derived logger with `worker_id`, `correlation_id`, `notification_id`, `channel` and `attempt`; a start line before the POST; a result line after it with status code, duration and outcome ([K21]). Tests: `TestLeaseDeadlineFromClaimStart`, `TestDeliverySent`, `TestAcceptedWithBrokenBody`, `TestAcceptedBodyTimeout`.
5. **Worker configuration**, with `TestWorkerConfig`.
6. **Shutdown.** On `SIGTERM`: stop claiming at once; let calls in flight finish (each bounded by its 5-second timeout) and write their results; exit when all are done or `SHUTDOWN_BUDGET` ends. Test: `TestWorkerShutdown`.
7. **Mock provider.** `POST /` → after `MOCK_DELAY` (default `0s`), `202` with `{messageId: <new UUID>, status: "accepted", timestamp: <RFC 3339>}`; a body that is not the expected JSON → `400`. It records each arrival's time per channel. `GET /stats` returns per channel the arrival count and the highest number of arrivals in any sliding one-second window. Test: `TestMockProvider`.
8. **End-to-end tests** in `internal/e2e`: `TestCreateWhileProviderBlocked`, `TestCorrelationIDEndToEnd`, `TestReplayAfterSent`.
9. **Image and Compose.** The Dockerfile now builds `api`, `worker`, `migrate` and `mockprovider`. New services:
   - `worker`: `command: ["/app/worker"]`; `PROVIDER_URL: ${PROVIDER_URL:-http://mockprovider:8081/}`; `PROVIDER_PROFILE: ${PROVIDER_PROFILE:-standard}`; after `postgres` is healthy and `migrate` completed; `stop_grace_period: 15s` ([K6]).
   - `mockprovider`: `command: ["/app/mockprovider"]`; `MOCK_DELAY: ${MOCK_DELAY:-0s}`, so a value set on a `docker compose up` command reaches the container; port `${BIND_ADDRESS:-127.0.0.1}:${MOCK_PORT:-8081}:8081` ([K39]).
10. **README:** architecture overview (API → `notifications` table → worker → provider), how to follow a notification to `sent`, the worker settings.

## Tests

| Test | Package | Kind | Setup | Asserts |
|---|---|---|---|---|
| `TestClaimSetsLease` | `internal/postgres` | Database | One queued row | `processing`, a token, an expiry in the future, `attempts` 1, a positive remaining duration |
| `TestSingleLease` | `internal/postgres` | Database race | 200 queued rows; two claimers on separate connections until empty | Each row claimed exactly once; tokens distinct |
| `TestResultRequiresValidLease` | `internal/postgres` | Database | Claimed row | Wrong token: no row changed. Expired lease by the database clock: no row changed. Status no longer `processing`: no row changed. Valid: one row changed and both lease columns empty |
| `TestDuplicateMessageID` | `internal/postgres` | Database | Two rows | Both store the same `provider_message_id` |
| `TestLeaseDeadlineFromClaimStart` | `internal/worker` | Unit, fake claimer | The fake claim takes 1 second and returns a remaining lease of 30 seconds | The worker's deadline is at most `t0 + 30 s`, at least 1 second earlier than the arrival of the answer plus 30 seconds |
| `TestDeliverySent` | `internal/worker` | Worker + database + `httptest` | One queued row; provider answers `202` with a valid body | Provider got one POST with `{to, channel, content}`, `Content-Type: application/json` and `X-Correlation-ID`; row `sent` with `sent_at` and both provider fields; lease cleared |
| `TestAcceptedWithBrokenBody` | `internal/worker` | Same | Provider answers `202` with invalid JSON | One POST; `sent`; provider fields empty; one warning line with the correlation ID |
| `TestAcceptedBodyTimeout` | `internal/worker` | Same | Provider sends the `202` status line, flushes, then never finishes the body | One POST; `sent` written after the HTTP timeout, within the lease; provider fields empty |
| `TestNoRedirect` | `internal/worker` | Same | Subtests `307` and `308`, `Location` pointing at a second `httptest` server | The second server receives no request; the row is not `sent` |
| `TestWorkerConfig` | `internal/config` | Unit | Variables through `Options.Environment` | Missing `PROVIDER_URL` rejected; unknown profile rejected; lease shorter than timeout plus budget rejected |
| `TestWorkerShutdown` | `internal/worker` | Worker + database + `httptest` | Ten queued rows; provider signals each arrival and answers 3 seconds later; one call slot; stop requested as soon as the first arrival is signalled | No second claim after the stop; the call in flight finishes within its 5-second timeout and its row becomes `sent`; the other nine stay `queued`; the worker returns within 10 seconds |
| `TestMockProvider` | `internal/mockprovider` | HTTP | Three valid POSTs, one malformed | `202` with three different `messageId`s in the case's shape; `400` for the malformed one; `/stats` counts arrivals per channel |
| `TestCreateWhileProviderBlocked` | `internal/e2e` | API + worker + `httptest` | Provider signals each arrival and holds every request until released. Create A; wait at most 5 seconds for A's arrival signal; then create B | A's arrival is signalled (the worker really called the provider); B gets `202` within 1 second while the provider still holds A; after the release both rows become `sent` |
| `TestCorrelationIDEndToEnd` | `internal/e2e` | Same, logs captured | Create with `X-Correlation-ID: test-123` | Provider got the header; the API's and the worker's log lines for the notification carry `correlation_id: test-123` |
| `TestReplayAfterSent` | `internal/e2e` | Same | Create with key `k1` and correlation ID `first`; wait for `sent`; repeat with key `k1` and correlation ID `second` | `GET` shows `sent`; the replay returns the first `queued` body byte for byte with `202`; the stored correlation ID is still `first`; the provider counted one POST |

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Lease and result rules ([R11]) | `go test -count=1 -v -run '^(TestClaimSetsLease\|TestSingleLease\|TestResultRequiresValidLease\|TestDuplicateMessageID)$' ./internal/postgres/` | Four `--- PASS` lines |
| Delivery ([R12], [R13]) | `go test -count=1 -v -run '^(TestLeaseDeadlineFromClaimStart\|TestDeliverySent\|TestAcceptedWithBrokenBody\|TestAcceptedBodyTimeout\|TestNoRedirect\|TestWorkerShutdown)$' ./internal/worker/` | Six `--- PASS` lines |
| End to end ([R7], [R15], [R11]) | `go test -count=1 -v -run '^(TestCreateWhileProviderBlocked\|TestCorrelationIDEndToEnd\|TestReplayAfterSent)$' ./internal/e2e/` | Three `--- PASS` lines |
| Mock and configuration | `go test -count=1 -v -run '^TestMockProvider$' ./internal/mockprovider/` and `go test -count=1 -v -run '^TestWorkerConfig$' ./internal/config/` | One `--- PASS` line each |
| Package boundaries ([K23]) | `go list -deps ./internal/service ./internal/worker \| grep -c 'internal/postgres'` | `0` |
| Through the stack | [Stack check](#stack-check) | As described there |

## Stack check

The check counts the mock's arrivals, so it runs in its own Compose project with an empty database and a fresh mock: notifications or counters from earlier runs cannot change the result. The provider and the mock's delay are set on the `up` command, so the shell's environment can neither send this notification to a real URL nor slow the mock into timeouts and retries that would add arrivals.

```bash
docker compose stop          # the main stack, if it runs; frees the host ports
P="nsys-delivery-$(date +%s)"
PROVIDER_URL=http://mockprovider:8081/ PROVIDER_PROFILE=standard MOCK_DELAY=0s \
  docker compose -p "$P" up -d --build
scripts/wait-health.sh -p "$P" api healthy 60
ID=$(curl -s -X POST http://localhost:8080/notifications \
  -H 'Content-Type: application/json' -H 'X-Correlation-ID: demo-1' \
  -d '{"recipient":"+905551234567","channel":"sms","content":"hello"}' | jq -r .id)
for i in $(seq 1 30); do
  STATUS=$(curl -s "http://localhost:8080/notifications/$ID" | jq -r .status)
  [ "$STATUS" = sent ] && break
  sleep 1
done
curl -s "http://localhost:8080/notifications/$ID" | jq '{status, provider_message_id, sent_at}'
docker compose -p "$P" logs --no-log-prefix worker | grep '"correlation_id":"demo-1"'
curl -s http://localhost:8081/stats | jq .
docker compose -p "$P" down -v
docker compose start         # the main stack, if it ran before
```

**Expected observation:** within 30 seconds `status` is `sent` with a non-empty `provider_message_id` (a UUID from the mock); the worker log shows one start line and one result line with `"correlation_id":"demo-1"`; `/stats` shows exactly one arrival, on `sms`. If the loop ends with any other status, the check fails.

## Failure scenarios

| Scenario | Covered by |
|---|---|
| Provider slow or stuck | `TestCreateWhileProviderBlocked` |
| `202` with an invalid body | `TestAcceptedWithBrokenBody` |
| `202` whose body never finishes | `TestAcceptedBodyTimeout` |
| Redirect to another address | `TestNoRedirect` |
| Two workers reaching for the same row | `TestSingleLease` |
| Late write after the lease ended | `TestResultRequiresValidLease` |
| Slow claim answer | `TestLeaseDeadlineFromClaimStart` |
| Stop signal with a call in flight | `TestWorkerShutdown`; stack variant below |

**Stack variant of the shutdown scenario:** a project as in the stack check, but with a mock that holds each request for 3 seconds. The worker is stopped only after it has started the call: the loop waits at most 5 seconds for the first worker log line of this notification, which is its start line, written before the POST. Stopping right after the create could stop a worker that has not claimed anything yet.

```bash
docker compose stop          # the main stack, if it runs; frees the host ports
P="nsys-shutdown-$(date +%s)"
PROVIDER_URL=http://mockprovider:8081/ PROVIDER_PROFILE=standard MOCK_DELAY=3s \
  docker compose -p "$P" up -d --build
scripts/wait-health.sh -p "$P" api healthy 60
ID=$(curl -s -X POST http://localhost:8080/notifications -H 'Content-Type: application/json' \
  -d '{"recipient":"+905551234567","channel":"sms","content":"in flight"}' | jq -r .id)
STARTED=no
for i in $(seq 1 25); do       # at most 5 s, every 0.2 s
  docker compose -p "$P" logs --no-log-prefix worker \
    | jq -eR --arg id "$ID" 'fromjson? | select(.notification_id == $id)' >/dev/null \
    && STARTED=yes && break
  sleep 0.2
done
echo "started=$STARTED"
docker compose -p "$P" stop worker
docker compose -p "$P" logs --no-log-prefix worker | tail -5
docker compose -p "$P" ps -a --format 'table {{.Service}}\t{{.State}}\t{{.ExitCode}}'
curl -s "http://localhost:8080/notifications/$ID" | jq -r .status
docker compose -p "$P" down -v
docker compose start         # the main stack, if it ran before
```

Expected: `started=yes`; the attempt's result line appears before the worker's last line; the worker shows `exited 0`; the notification is `sent`. With `started=no` the check fails, because the stop did not test a call in flight.

## Evidence for the pull request

- Output of every acceptance check and of the stack check.
- The interim rule for non-`202` responses, stated in the pull request so the reviewer knows slice 4 replaces it.

## Documentation

- `README.md`: architecture overview, worker settings, how to follow a notification, the mock and `/stats`.
- `AI_USAGE.md`: slice log row; evidence rows for [R7], [R11] (lease, replay), [R12] (calls), [R13] (mock), [R15] (end to end), [N2] and [A2].

[R7]: ../REQUIREMENTS.md#processing-engine
[R11]: ../REQUIREMENTS.md#processing-engine
[R12]: ../REQUIREMENTS.md#delivery--retry-left-to-the-candidates-design
[R13]: ../REQUIREMENTS.md#external-provider
[R15]: ../REQUIREMENTS.md#observability
[N2]: ../REQUIREMENTS.md#non-functional-requirements-from-the-case-text
[A2]: ../REQUIREMENTS.md#assumptions
[K2]: ../DECISIONS.md#k2--queue-the-notifications-table-in-postgresql-is-the-queue
[K3]: ../DECISIONS.md#k3--delivery-guarantee-a-durable-queue-lease-recovery-and-bounded-retries
[K5]: ../DECISIONS.md#k5--rate-limiter-one-shared-permit-row-per-channel-in-postgresql
[K6]: ../DECISIONS.md#k6--process-layout-the-api-and-the-worker-run-as-separate-processes-from-one-go-module
[K7]: ../DECISIONS.md#k7--retry-policy-retry-by-error-type-exponential-backoff-with-jitter-five-attempts-in-total
[K9]: ../DECISIONS.md#k9--status-model-and-cancellation-five-statuses-and-pending-means-queued
[K10]: ../DECISIONS.md#k10--idempotency-key-an-optional-idempotency-key-header-one-key-per-create-request
[K14]: ../DECISIONS.md#k14--provider-a-small-go-mock-by-default-webhooksite-as-a-separate-profile
[K15]: ../DECISIONS.md#k15--provider-response-store-messageid-and-timestamp
[K21]: ../DECISIONS.md#k21--logging-logslog-json-with-a-correlation-id-stored-on-each-notification
[K23]: ../DECISIONS.md#k23--project-layout-small-packages-by-layer-with-multi-table-transactions-inside-postgres
[K31]: ../DECISIONS.md#k31--image-d3-a-multi-stage-build-on-a-distroless-non-root-image
[K32]: ../DECISIONS.md#k32--configuration-typed-environment-variables-with-caarlos0env
[K39]: ../DECISIONS.md#k39--host-ports-configurable-bound-to-127001
