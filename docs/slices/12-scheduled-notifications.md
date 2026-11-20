# Slice 12 — Scheduled notifications

[Delivery plan](../PLAN.md) · Branch `feat/scheduled` · Optional ([B2]) · Depends on: slices 6, 8, 9 and 10

## Goal

A notification with `scheduled_at` is not claimed before that time, and the metrics separate a planned wait from delivery latency. Built before the WebSocket updates ([K26], [K24]).

## What this slice delivers

| ID | Delivered here |
|---|---|
| [B2] | Optional absolute `scheduled_at` on single and batch requests ([K26]) |

## Preconditions

- Slices 6, 8, 9 and 10 merged.

## Files

| Path | Purpose |
|---|---|
| `migrations/00009_scheduled_at.sql` | `notifications.scheduled_at timestamptz`, null when not scheduled |
| `internal/domain/` | Parsing and the past-time rule |
| `internal/service/` | Validation order |
| `internal/postgres/` | First `available_at` from `scheduled_at`; latency split in the metrics |
| `internal/httpapi/` | The field on both create operations |
| `api/openapi.yaml` | Regenerated |

## Rules ([K26])

| Topic | Rule |
|---|---|
| Format | RFC 3339 with an offset (`Z` or `±hh:mm`); a value without an offset gets `422` |
| Storage | Normalized to UTC in `scheduled_at`; the first `available_at` is the same time |
| Retries | Change only `available_at`; `scheduled_at` is kept |
| Validation order | Field validation → request hash (which includes `scheduled_at`) → key → only for a new key, the past-time rule |
| Past time | A new request with a time at or before the database clock gets `422` |
| Upper limit | None |
| Everything else | Cancel ([K9]), priority ([K8]) and the rate limit ([K5]) work as for any queued notification; a waiting scheduled notification counts as `deferred` ([K20]) |
| Latency | Two distributions, each with its sample count: `created_at → sent_at` for immediate notifications and `scheduled_at → sent_at` for scheduled ones |
| Order guarantee in a batch | Only between items with the same channel and the same priority; items on different channels or priorities are ordered by [K5] and [K8] first |

## Steps

1. **Migration** `00009`.
2. **Parsing and rules** in `internal/domain`, with `TestScheduledAtParsing`.
3. **Validation order** in `internal/service`.
4. **Insert** sets `available_at` from `scheduled_at`; retries keep `scheduled_at`.
5. **Metrics:** split latency, `TestMetricsLatencySplit`.
6. **Tests** below; OpenAPI regenerated.
7. **README:** scheduling example; the client-clock note; no upper limit ([K26]).

## Tests

| Test | Package | Kind | Setup | Asserts |
|---|---|---|---|---|
| `TestScheduledAtParsing` | `internal/domain` | Unit, table | `Z`, `+03:00`, no offset, garbage | First two accepted and normalized to UTC; the others rejected |
| `TestScheduledNotClaimedEarly` | `internal/postgres` | Database | Scheduled 3 seconds ahead | Claim before the time returns nothing; after it, the row is claimed |
| `TestScheduledCancel` | `internal/e2e` | API + worker + `httptest` | Scheduled 3 seconds ahead; cancel at once; wait 5 seconds | Cancel `200`; no POST |
| `TestScheduledPastTime` | `internal/httpapi` | HTTP + database | Time one second in the past, new key | `422` |
| `TestScheduledRepeatAfterTime` | `internal/httpapi` | HTTP + database | Created 2 seconds ahead with a key; repeated 3 seconds later | First response bytes; no new row |
| `TestScheduledKeyConflict` | `internal/httpapi` | HTTP + database | Same key, different `scheduled_at` | `422` |
| `TestRetryKeepsScheduledAt` | `internal/worker` | Worker + `httptest` | Scheduled row; provider answers `503` once | `available_at` moves; `scheduled_at` unchanged |
| `TestScheduledBatchOrder` | `internal/postgres` | Database | One batch: three `sms` `normal` items scheduled at t+3, t+1, t+2 seconds | Claimed in the order t+1, t+2, t+3 |
| `TestMetricsLatencySplit` | `internal/postgres` | Database, fixed `as_of` | Sent immediate and scheduled rows with known times | Two distributions with their own sample counts; a scheduled row's planned wait is absent from the immediate one |

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Parsing | `go test -count=1 -v -run '^TestScheduledAtParsing$' ./internal/domain/` | `--- PASS` |
| Not before its time ([B2]) | `go test -count=1 -v -run '^(TestScheduledNotClaimedEarly\|TestScheduledBatchOrder\|TestMetricsLatencySplit)$' ./internal/postgres/` | Three `--- PASS` lines |
| API rules | `go test -count=1 -v -run '^(TestScheduledPastTime\|TestScheduledRepeatAfterTime\|TestScheduledKeyConflict)$' ./internal/httpapi/` | Three `--- PASS` lines |
| Cancel and retry | `go test -count=1 -v -run '^TestScheduledCancel$' ./internal/e2e/` and `go test -count=1 -v -run '^TestRetryKeepsScheduledAt$' ./internal/worker/` | One `--- PASS` line each |
| Through the stack | [Stack check](#stack-check) | As described there |

## Stack check

```bash
AT=$(date -u -v+10S +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '+10 seconds' +%Y-%m-%dT%H:%M:%SZ)
ID=$(curl -s -X POST http://localhost:8080/notifications -H 'Content-Type: application/json' \
  -d "{\"recipient\":\"+905551234567\",\"channel\":\"sms\",\"content\":\"later\",\"scheduled_at\":\"$AT\"}" | jq -r .id)
sleep 5;  curl -s "http://localhost:8080/notifications/$ID" | jq -r .status
sleep 10; curl -s "http://localhost:8080/notifications/$ID" | jq '{status, scheduled_at, sent_at}'
SCHED=$(curl -s "http://localhost:8080/notifications/$ID" | jq -r .scheduled_at)
START=$(docker compose logs --no-log-prefix worker \
  | jq -rR --arg id "$ID" 'fromjson? | select(.notification_id == $id and has("started_at")) | .started_at' | head -1)
echo "scheduled_at=$SCHED started_at=$START"
jq -n --arg s "$SCHED" --arg t "$START" \
  '($t | sub("\\.[0-9]+"; "") | fromdateiso8601) >= ($s | sub("\\.[0-9]+"; "") | fromdateiso8601)'
```

`sent_at` is written when the answer arrives, so `sent_at` after `scheduled_at` would not show that the POST left on time. The check therefore reads the send start from the worker's start line (`started_at`, slice 5, [K42]). The mock's `/stats` holds counts only, not one notification's arrival time. Fractions of a second are cut before the comparison; `scheduled_at` is in whole seconds here, so cutting them cannot turn an early start into a pass. `TestScheduledNotClaimedEarly` stays the repository proof.

**Expected observation:** `queued` after 5 seconds; `sent` after 15 seconds; the start line exists, and the last command prints `true`: the send started at or after `scheduled_at`.

## Evidence for the pull request

- Output of every acceptance check and of the stack check.

## Documentation

- `README.md`: scheduling; the bonus listed as done only after this pull request merges.
- `AI_USAGE.md`: slice log row; evidence row for [B2].

[B2]: ../REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[K5]: ../DECISIONS.md#k5--rate-limiter-one-shared-permit-row-per-channel-in-postgresql
[K8]: ../DECISIONS.md#k8--priority-strict-order-within-each-channel
[K9]: ../DECISIONS.md#k9--status-model-and-cancellation-five-statuses-and-pending-means-queued
[K20]: ../DECISIONS.md#k20--metrics-a-json-endpoint-computed-from-postgresql
[K24]: ../DECISIONS.md#k24--real-time-status-events-b4-postgresql-listennotify
[K26]: ../DECISIONS.md#k26--scheduled-notifications-b2-an-optional-absolute-scheduled_at
[K42]: ../DECISIONS.md#k42--send-starts-a-permit-with-a-start-deadline
