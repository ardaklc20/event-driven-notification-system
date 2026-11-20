# Slice 4 — Retries and crash recovery

[Delivery plan](../PLAN.md) · Branch `feat/retries-recovery` · Core · Depends on: slice 3

## Goal

Temporary failures are retried with backoff, a permanent failure stops the flow on the attempt where it arrives, a crashed worker's notification is taken back when its lease expires, and a worker whose lease has ended can never change the row.

## What this slice delivers

| ID | Delivered here |
|---|---|
| [R11] | The second and third tests of [K3]: a stale worker cannot overwrite; an unclear outcome leads to a second call |
| [R12] | [K7]'s classification, backoff, `Retry-After`, five attempts |
| [R13] | The webhook.site integration check ([K14]) |
| [N3] | Lease recovery after a worker crash ([K3], [K9]) |

## Preconditions

- Slice 3 merged.
- Recovery follows [K40] (a bounded step in every worker); responses [K7] does not list follow [K41] (terminal protocol errors).

## Files

| Path | Purpose |
|---|---|
| `internal/provider/` | Classification, `Retry-After` parsing, profiles ([K7], [K14]) |
| `internal/worker/` | Backoff, result handling for temporary and permanent outcomes, recovery step |
| `internal/postgres/` | Retry write, failure write, recovery statement |
| `internal/config/` | Retry and recovery settings ([K32]) |
| `scripts/webhook-check.sh` | The webhook.site integration check; stops at the first failure and exits with 1 |

No migration: every column this slice needs exists.

## Classification ([K7])

| Outcome | Class | Write |
|---|---|---|
| `202` | Accepted | `sent` (on any attempt, the fifth included) |
| Network error, timeout, `408`, `500`, `502`, `503`, `504` | Temporary | Attempts left: `queued`, `available_at` = backoff or `Retry-After`, `last_error`. None left: `failed` |
| `429` under `standard` | Temporary | As above, `Retry-After` respected |
| `429` under `webhook_site_free` | Permanent | `failed` |
| `400`, `501` | Permanent | `failed` on this attempt, whatever attempts are left |
| Any other code: another `2xx`, a `3xx` (never followed, [K14]), another `4xx`, another `5xx` | Terminal protocol error ([K41]) | `failed` on this attempt, whatever attempts are left; `last_error` holds the class and the code, for example `terminal: unexpected status 200` |

Every write keeps slice 3's conditions (`processing`, matching token, lease valid by the database clock) and clears the lease fields.

## Backoff ([K7])

- The n-th retry waits a random time in `[0, min(60 s, 2 s × 2^(n−1))]`.
- A valid `Retry-After` (seconds or an HTTP date) that asks for longer wins, beyond the 60-second cap. An invalid one is ignored.
- `available_at` is set from the database clock plus the wait.

## Lease recovery ([K3], [K9])

- Finds rows in `processing` whose `lease_expires_at` has passed by the database clock.
- Attempts left: `queued`, lease fields cleared, `last_error` notes the expired lease.
- No attempts left: `failed`, lease fields cleared.
- Takes no rate-limit permit and does not consult the circuit breaker; it only changes statuses.
- Runs inside every worker ([K40]): every `RECOVERY_INTERVAL` one statement takes at most `RECOVERY_BATCH` expired leases with `FOR UPDATE SKIP LOCKED`, so workers that recover at the same time never take the same row, and a large backlog is worked off over several rounds.
- With a 30-second lease, a crashed worker's notification is expected back in `queued` within about 35 seconds while the workers run. That is an expectation, not a bound: nothing is recovered while no worker runs ([K40]).

## Settings ([K32])

| Variable | Default |
|---|---|
| `MAX_ATTEMPTS` | `5` |
| `BACKOFF_BASE` | `2s` |
| `BACKOFF_CAP` | `60s` |
| `RECOVERY_INTERVAL` | `5s` ([K40]) |
| `RECOVERY_BATCH` | `100` ([K40]) |

`Validate()` rejects an interval that is not positive and a batch below 1.

## Steps

1. **Classification and `Retry-After`** as pure functions: `TestClassify`, `TestParseRetryAfter`.
2. **Backoff** as a pure function: `TestBackoff`.
3. **Result handling** for the three classes, replacing slice 3's interim rule: the attempt-sequence tests below.
4. **Attempt limit in the claim:** only rows with `attempts < MAX_ATTEMPTS` are claimed.
5. **Recovery step** in every worker ([K40]): `TestLeaseRecovery`, `TestRecoveryConcurrent`, `TestRecoveryLoop`, `TestLate202AfterFailed`, `TestStaleWriteAfterRecovery`.
6. **Crash tests:** `TestExpiredLeaseCannotOverwrite`, `TestAttemptCountedOnCrash`, `TestSendDeadlineBeforeLease`, `TestUnclearOutcomeRetries`.
7. **webhook.site check**, below.
8. **README:** the retry policy, the provider profiles, the duplicate-send limit ([K3]).

Crashes are simulated inside the test process: the test stops a worker's loop at a chosen point (after its claim, after the provider has received the POST, or right before its result write) and gives it a short lease so that recovery happens within the test. A stopped worker either never writes, or is released later so that its late write is really attempted.

## Tests

| Test | Package | Kind | Setup | Asserts |
|---|---|---|---|---|
| `TestClassify` | `internal/provider` | Unit, table | Every row of the classification table, under both profiles; for [K41] the codes `200`, `204`, `301`, `307`, `404`, `409` and `505` | Expected class per row; each [K41] code is terminal, and its `last_error` text names the class and the code |
| `TestParseRetryAfter` | `internal/provider` | Unit, table | Seconds, HTTP date, past date, garbage | Parsed durations; garbage ignored |
| `TestBackoff` | `internal/worker` | Unit | Attempts 1–4, many samples each; a longer `Retry-After` | Every wait within its bound; `Retry-After` wins when longer |
| `TestRetryThenSent` | `internal/worker` | Worker + database + `httptest` | `503`, then `202`; zero backoff | Two POSTs; `sent`; `attempts` 2 |
| `TestSentOnFifthAttempt` | same | same | Four `503`s, then `202` | Five POSTs; `sent`; `attempts` 5 |
| `TestFailsAfterFiveTemporary` | same | same | Five `503`s | Five POSTs; `failed`; `last_error` names `503` |
| `TestPermanentAfterTemporary` | same | same | `503`, then `400` | Two POSTs; `failed` after the second |
| `TestPermanentErrorFailsOnce` | same | same | Subtests: `400`; `200`, a `2xx` other than `202` ([K41]) | One POST in each; `failed`; for `200`, `last_error` is `terminal: unexpected status 200` and no second POST follows |
| `TestRetryAfterRespected` | same | same | `503` with `Retry-After: 2`, then `202`; the provider records arrival times | No second POST arrives earlier than 2 seconds after the first response; `available_at` at least 2 seconds after it |
| `TestTooManyRequestsByProfile` | same | same | `429`; once per profile | `webhook_site_free`: one POST, `failed`. `standard`: retried |
| `TestLeaseRecovery` | `internal/worker` | Crash simulation | Worker A claims and stops before the POST; short lease | Recovery returns the row to `queued`; worker B claims it with a new token; it ends `sent` |
| `TestExpiredLeaseCannotOverwrite` | `internal/postgres` | Database | A's lease expires; B claims; A writes its result | A's write changes no row; B's state stays |
| `TestRecoveryConcurrent` | `internal/postgres` | Database | 150 rows in `processing` with attempts left and expired leases; two recovery statements with `RECOVERY_BATCH` 100 run at the same time on two connections | Together they change 150 distinct rows, none twice: one changes at most 100, the other the rest; every row is `queued` with its lease fields cleared and its `attempts` unchanged |
| `TestRecoveryLoop` | `internal/worker` | Worker + database | A running worker with `RECOVERY_INTERVAL` 100 ms; the test inserts a row with an expired lease and never calls recovery itself | Within 2 seconds the row is back in `queued`; after the worker stops, the loop has ended (no further recovery query) |
| `TestLate202AfterFailed` | `internal/worker` | Crash simulation | Fifth attempt with a short lease: the provider answers `202` at once, while the lease is still valid; a hook stops the worker right before its result write; the test lets the lease expire and runs recovery, which marks the row `failed`; then it releases the hook | The late result is not recorded: the row stays `failed`; `sent_at` and the provider fields stay empty; no further POST. Either way the worker ends is accepted: its result context, bounded by the lease ([K15]), has already ended before the SQL runs, or its `UPDATE` changes no row |
| `TestStaleWriteAfterRecovery` | `internal/postgres` | Database | A row claimed at its fifth attempt with a short lease; the lease expires; recovery marks it `failed`; then the result write runs with the old token and a fresh context | The `UPDATE` changes 0 rows; the row is unchanged: `failed`, no `sent_at`, no provider fields |
| `TestUnclearOutcomeRetries` | `internal/worker` | Worker + `httptest` | Provider accepts the first request and answers only after the timeout; answers the second at once | Two calls recorded for the same notification; final `sent` |
| `TestAttemptCountedOnCrash` | `internal/worker` | Crash simulation | Worker stops right after each POST arrives, five times | Each attempt counted; after the fifth lease expires the row is `failed`; the provider never receives a sixth POST |
| `TestSendDeadlineBeforeLease` | `internal/worker` | Worker + `httptest` | Remaining lease shorter than the result budget | No POST from that worker |

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Classification and backoff | `go test -count=1 -v -run '^(TestClassify\|TestParseRetryAfter)$' ./internal/provider/` and `go test -count=1 -v -run '^TestBackoff$' ./internal/worker/` | Three `--- PASS` lines |
| Attempt sequences ([R12]) | `go test -count=1 -v -run '^(TestRetryThenSent\|TestSentOnFifthAttempt\|TestFailsAfterFiveTemporary\|TestPermanentAfterTemporary\|TestPermanentErrorFailsOnce\|TestRetryAfterRespected\|TestTooManyRequestsByProfile)$' ./internal/worker/` | Seven `--- PASS` lines |
| Crash recovery ([N3]) | `go test -count=1 -v -run '^(TestLeaseRecovery\|TestRecoveryLoop\|TestLate202AfterFailed\|TestAttemptCountedOnCrash\|TestSendDeadlineBeforeLease)$' ./internal/worker/` | Five `--- PASS` lines |
| Recovery in several workers ([K40]) | `go test -count=1 -v -run '^TestRecoveryConcurrent$' ./internal/postgres/` | `--- PASS: TestRecoveryConcurrent` |
| Stale worker ([R11], [K3] test 2) | `go test -count=1 -v -run '^(TestExpiredLeaseCannotOverwrite\|TestStaleWriteAfterRecovery)$' ./internal/postgres/` | Two `--- PASS` lines |
| Unclear outcome ([R11], [K3] test 3) | `go test -count=1 -v -run '^TestUnclearOutcomeRetries$' ./internal/worker/` | `--- PASS: TestUnclearOutcomeRetries` |
| webhook.site ([R13]) | [webhook.site integration check](#webhooksite-integration-check) | As described there |

## webhook.site integration check

A manual check against a real webhook.site URL, run by `scripts/webhook-check.sh`. The script runs **its own Compose project, with an empty database, one worker and one synthetic notification**, so no other notification can reach the external URL. A free URL accepts 100 requests and lives 7 days, and creating one through the API is limited to one per minute ([K14]). Anyone who knows a free URL's ID can read what it received, so the script never prints the full URL; it prints only the token's first group.

**What the script does.** It runs with `set -euo pipefail`. Each step below either passes or prints `FAIL: <step>` and exits with 1. A trap removes the script's own project (`down -v` on that project only) on every exit.

1. **Host ports.** The script reads the same variables as Compose ([K39]): `POSTGRES_PORT`, `API_PORT` and `MOCK_PORT` (5433, 8080 and 8081 by default) must be free; otherwise it fails with "stop the main stack first". It calls the API at `http://${BIND_ADDRESS:-127.0.0.1}:${API_PORT:-8080}`.
2. **Names.** Project `nsys-webhook-<epoch>`, correlation ID `wh-check-<epoch>`, `messageId` `msg-<correlation ID>`, provider timestamp = the run's start time in UTC.
3. **URL.** `POST https://webhook.site/token` (`curl --max-time 15`) with `default_status` 202, `default_content_type` `application/json` and a body in the case's shape carrying this run's `messageId` and timestamp. No token → fail.
4. **Stack.** `docker compose -p <project> up -d --build` with `PROVIDER_URL=https://webhook.site/<token>` and `PROVIDER_PROFILE=webhook_site_free` set on that command, then `scripts/wait-health.sh -p <project> api healthy 60`; exit 1 → fail.
5. **Preconditions, enforced.** Exactly one `worker` container, and `select count(*) from notifications` returns 0.
6. **One notification.** `POST /notifications` with the run's correlation ID and `{"recipient":"+905551234567","channel":"sms","content":"webhook.site check"}`; poll `GET` at most 30 times, one second apart, until `sent` or `failed`.
7. **Our side.** `status` is `sent`; `provider_message_id` equals the run's `messageId`; `provider_timestamp` equals the run's timestamp.
8. **webhook.site's side.** Read `GET https://webhook.site/token/<token>/requests?sorting=newest` (`curl --max-time 15`). Exactly one request carries the run's `x-correlation-id`; its method is `POST`; its body, parsed as JSON and compared with `jq -S`, equals `{"to":"+905551234567","channel":"sms","content":"webhook.site check"}`.
9. **Evidence and result.** Prints the UTC date, the masked URL, the correlation ID, the payload, the final status and the `messageId`, then `PASS`, and exits with 0.

**Run:**

```bash
docker compose stop          # the main stack; frees the host ports, keeps its volume
scripts/webhook-check.sh; echo "exit=$?"
docker compose start
```

**Expected observation:** the last lines are `PASS` and `exit=0`. Any failed step prints `FAIL: <step>` and gives `exit=1`; a missing token, a timeout, a second worker, a non-empty database, a wrong `messageId`, a missing or duplicated request at webhook.site, a method other than `POST` or a different body all fail. The HTTPS call from the distroless image also proves its CA certificates ([K31]).

**Evidence:** the script's output, which holds only the masked URL, goes into the pull request. This slice's run is the one the README records (date, masked URL, correlation ID, final status, `messageId`). Later release checks put their own runs in their pull requests, not in the README.

## Failure scenarios

| Scenario | Covered by |
|---|---|
| Temporary failures, then success, including on the fifth attempt | `TestRetryThenSent`, `TestSentOnFifthAttempt` |
| Attempts run out | `TestFailsAfterFiveTemporary` |
| Permanent error after a temporary one | `TestPermanentAfterTemporary` |
| Provider asks to wait | `TestRetryAfterRespected` |
| Quota exhausted on webhook.site | `TestTooManyRequestsByProfile` |
| Worker crash before the result | `TestLeaseRecovery` |
| Crash after the POST, five times | `TestAttemptCountedOnCrash` |
| Stale worker writes after another took over | `TestExpiredLeaseCannotOverwrite` |
| Stale worker writes after recovery failed the row | `TestLate202AfterFailed` (worker), `TestStaleWriteAfterRecovery` (the `UPDATE` itself) |
| Provider accepted, response lost | `TestUnclearOutcomeRetries` |
| Provider answers with a code [K7] does not list | `TestClassify`, `TestPermanentErrorFailsOnce` ([K41]) |
| Two workers recover expired leases at the same time | `TestRecoveryConcurrent` ([K40]) |
| Lease too short to send | `TestSendDeadlineBeforeLease` |

## Evidence for the pull request

- Output of every acceptance check.
- The output of `scripts/webhook-check.sh`: date, masked URL, correlation ID, payload, final status, `messageId`, `PASS`. Never the full URL or token.

## Documentation

- `README.md`: retry policy, profiles, the `failed` meaning ([K7]), the duplicate-send limit ([K3]), how to run `scripts/webhook-check.sh`, and this slice's run (date, masked URL, correlation ID, final status, `messageId`).
- `AI_USAGE.md`: slice log row; evidence rows for [R11] (second and third tests), [R12], [R13] (webhook.site) and [N3].

[R11]: ../REQUIREMENTS.md#processing-engine
[R12]: ../REQUIREMENTS.md#delivery--retry-left-to-the-candidates-design
[R13]: ../REQUIREMENTS.md#external-provider
[N3]: ../REQUIREMENTS.md#non-functional-requirements-from-the-case-text
[K3]: ../DECISIONS.md#k3--delivery-guarantee-a-durable-queue-lease-recovery-and-bounded-retries
[K7]: ../DECISIONS.md#k7--retry-policy-retry-by-error-type-exponential-backoff-with-jitter-five-attempts-in-total
[K9]: ../DECISIONS.md#k9--status-model-and-cancellation-five-statuses-and-pending-means-queued
[K14]: ../DECISIONS.md#k14--provider-a-small-go-mock-by-default-webhooksite-as-a-separate-profile
[K15]: ../DECISIONS.md#k15--provider-response-store-messageid-and-timestamp
[K31]: ../DECISIONS.md#k31--image-d3-a-multi-stage-build-on-a-distroless-non-root-image
[K32]: ../DECISIONS.md#k32--configuration-typed-environment-variables-with-caarlos0env
[K39]: ../DECISIONS.md#k39--host-ports-configurable-bound-to-127001
[K40]: ../DECISIONS.md#k40--lease-recovery-a-bounded-step-in-every-worker
[K41]: ../DECISIONS.md#k41--responses-k7-does-not-list-terminal-protocol-errors
