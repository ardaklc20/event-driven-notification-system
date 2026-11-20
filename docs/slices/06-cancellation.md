# Slice 6 — Cancellation

[Delivery plan](../PLAN.md) · Branch `feat/cancel` · Core · Depends on: slice 5

## Goal

`POST /notifications/{id}/cancel` stops a `queued` notification for good. A cancel racing a claim, or racing a worker's result write, always ends with exactly one outcome, and no new POST follows a successful cancel.

## What this slice delivers

| ID | Delivered here |
|---|---|
| [R5] | Cancel of a pending (`queued`) notification, with the responses [K9] defines |

## Preconditions

- Slice 5 merged: the races are tested against the final claim transaction, which holds the permit ([K5]), and against retry writes from slice 4.

## Files

| Path | Purpose |
|---|---|
| `internal/postgres/` | The cancel transaction |
| `internal/service/` | Cancel use case |
| `internal/httpapi/` | Cancel operation |
| `api/openapi.yaml` | Regenerated |

No migration.

## The cancel transaction ([K9])

One short transaction:

1. `SELECT status FROM notifications WHERE id = $1 FOR UPDATE`.
2. No row → roll back; `404`.
3. `queued` → update to `cancelled`, commit; `200` with the notification.
4. `cancelled` → commit; `200` with the notification.
5. `processing`, `sent` or `failed` → commit; `409` with the current status.

Reading and deciding under the row lock means the status cannot change between the decision and the answer.

## Races and their outcomes

| Race | First to lock the row | Outcome |
|---|---|---|
| Cancel vs claim | Cancel | Claim skips the row (`SKIP LOCKED`); row `cancelled`; no POST |
| Cancel vs claim | Claim | Cancel waits, reads `processing`, answers `409`; the claim's POST goes ahead |
| Cancel vs result write `processing → queued` | Result write | Cancel waits, reads `queued`, cancels; `200`; no further POST |
| Cancel vs result write `processing → queued` | Cancel | Cancel reads `processing`, answers `409`; then the row returns to `queued` and is retried |

Race tests use committed data, separate connections and controlled steps instead of `sleep` ([K22]). Tests in package `postgres` run the claim, the result write and the cancel as steps on transactions they open, hold and commit themselves.

## Steps

1. **Cancel transaction** and its repository tests.
2. **Service and operation:** `POST /notifications/{id}/cancel`, responses `200`, `404`, `409` with `{status}`; OpenAPI regenerated.
3. **Race tests**, both orders for each race.
4. **End-to-end tests:** no POST after a successful cancel; cancel during a retry wait.
5. **README:** cancel example; what "pending" means ([K9]).

## Tests

| Test | Package | Kind | Setup | Asserts |
|---|---|---|---|---|
| `TestCancelResponses` | `internal/httpapi` | HTTP + database | Rows in `queued`, `cancelled`, `processing`, `sent`, `failed`; an unknown ID | `200` `cancelled`; `200`; `409` with `processing`; `409` with `sent`; `409` with `failed`; `404` |
| `TestCancelClaimRace` | `internal/postgres` | Database race | Subtest *cancel first*: transaction A locks and cancels, holds; the claim runs on B; A commits. Subtest *claim first*: B claims and holds; the cancel starts on A and waits; B commits | *Cancel first:* B's claim returns no notification; row `cancelled`, `attempts` unchanged. *Claim first:* the cancel answers `409` with `processing`; row `processing` with B's token |
| `TestCancelResultRace` | `internal/postgres` | Database race | A claimed row. Subtest *result first*: the worker's `processing → queued` write holds; the cancel waits; the write commits. Subtest *cancel first*: the cancel locks and reads `processing`; the write waits; the cancel commits | *Result first:* cancel `200`, row `cancelled`, lease fields empty. *Cancel first:* cancel `409` with `processing`; the write then commits; row `queued` |
| `TestCancelClaimRaceRepeated` | `internal/postgres` | Database race | 200 runs, each with a fresh queued row; cancel and claim started together | Every run ends with exactly one of: `cancelled` and no claim, or claimed and `409`; the test logs how many runs ended each way |
| `TestNoPostAfterCancel` | `internal/e2e` | API + worker + `httptest` | Worker paused; create; cancel; worker resumed for longer than one claim cycle | Cancel `200`; the provider receives no POST for that notification |
| `TestCancelDuringRetryWait` | `internal/e2e` | API + worker + `httptest` | Provider answers `503` once; the row returns to `queued` with a later `available_at`; cancel before that time | Cancel `200`; after `available_at` has passed, no further POST arrives |

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Responses ([R5]) | `go test -count=1 -v -run '^TestCancelResponses$' ./internal/httpapi/` | `--- PASS: TestCancelResponses` |
| Races ([R5]) | `go test -count=1 -v -run '^(TestCancelClaimRace\|TestCancelResultRace\|TestCancelClaimRaceRepeated)$' ./internal/postgres/` | Three `--- PASS` lines, with every subtest passing |
| No POST after cancel ([R5]) | `go test -count=1 -v -run '^(TestNoPostAfterCancel\|TestCancelDuringRetryWait)$' ./internal/e2e/` | Two `--- PASS` lines |
| Through the stack | [Stack check](#stack-check) | As described there |

## Stack check

```bash
docker compose stop worker
ID=$(curl -s -X POST http://localhost:8080/notifications -H 'Content-Type: application/json' \
  -d '{"recipient":"+905551234567","channel":"sms","content":"cancel me"}' | jq -r .id)
curl -s -w '\n%{http_code}\n' -X POST "http://localhost:8080/notifications/$ID/cancel"
curl -s -w '\n%{http_code}\n' -X POST "http://localhost:8080/notifications/$ID/cancel"
docker compose start worker
sleep 5
curl -s "http://localhost:8080/notifications/$ID" | jq '{status, attempts}'
```

**Expected observation:** both cancels print `cancelled` and `200`; after the worker runs for 5 seconds the notification is still `cancelled` with `attempts: 0`.

## Evidence for the pull request

- Output of every acceptance check, including the outcome counts of `TestCancelClaimRaceRepeated`.
- Output of the stack check.

## Documentation

- `README.md`: cancel example and responses; "pending" means `queued`; batch cancel is not offered ([K9]).
- `AI_USAGE.md`: slice log row; evidence row for [R5].

[R5]: ../REQUIREMENTS.md#notification-management-api
[K5]: ../DECISIONS.md#k5--rate-limiter-one-shared-permit-row-per-channel-in-postgresql
[K9]: ../DECISIONS.md#k9--status-model-and-cancellation-five-statuses-and-pending-means-queued
[K22]: ../DECISIONS.md#k22--test-database-testcontainers-with-the-same-postgresql-image-as-compose
