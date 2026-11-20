# Slice 9 — Metrics

[Delivery plan](../PLAN.md) · Branch `feat/metrics` · Core · Depends on: slice 4

## Goal

`GET /metrics` reports, per channel, queue depth, the `processing` count, success and failure over the last 60 seconds and end-to-end latency percentiles, all read from one database snapshot.

## What this slice delivers

| ID | Delivered here |
|---|---|
| [R14] | Queue depth, success and failure rates and latency, "in real time" as of one snapshot ([K20]) |
| — | The `processing` count that [K30] relies on to show a stalled worker |
| — | The starvation signal [K8] relies on: the age of the oldest ready notification per priority |

## Preconditions

- Slice 4 merged (failed and retried notifications exist).

## Files

| Path | Purpose |
|---|---|
| `migrations/00007_completed_at.sql` | Schema below |
| `internal/postgres/` | Result writes set `completed_at`; the metrics snapshot queries |
| `internal/service/` | Assembles the response |
| `internal/httpapi/` | `GET /metrics` as a Huma operation ([K19]) |
| `api/openapi.yaml` | Regenerated |

## Schema: `00007_completed_at`

| Object | Content |
|---|---|
| `notifications.completed_at` | `timestamptz`, null until `sent` or `failed` |
| Index | `notifications (completed_at)` where `completed_at` is not null |
| Backfill | In the same migration: existing `sent` rows get `completed_at = sent_at`; existing `failed` rows get `completed_at = updated_at` |

Without the backfill, rows completed shortly before the migration would be missing from the first window after it.

Writes that change with this slice:

- `sent`: `completed_at` gets the same database time as `sent_at` (one `clock_timestamp()` value used for both).
- `failed`, from a result or from lease recovery: `completed_at = clock_timestamp()`, the same value as `updated_at` (recovery reads the clock once per statement for both).

## The snapshot ([K20])

1. `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY`.
2. First statement: `SELECT clock_timestamp()` → `as_of`. This statement also takes the snapshot.
3. Every later query receives `as_of` as a parameter; none calls `now()` or `clock_timestamp()` again.
4. Commit.

The window queries take `as_of` as a parameter, so tests can pass a fixed value and place rows exactly on the window's edges.

## Response, per channel

| Field | Definition |
|---|---|
| `as_of` | Top level; RFC 3339 with microseconds |
| `queue.<priority>.ready` | `queued` with `available_at <= as_of` |
| `queue.<priority>.deferred` | `queued` with `available_at > as_of` |
| `queue.<priority>.oldest_ready_age_seconds` | `as_of − min(available_at)` over ready rows; `null` when none |
| `processing` | Rows in `processing` |
| `completed.window_seconds` | `60` |
| `completed.sent`, `completed.failed` | Rows with `completed_at` in `(as_of − 60 s, as_of]` |
| `completed.sent_per_second` | `sent / 60`; `0` when nothing was sent |
| `completed.failed_per_second` | `failed / 60`; `0` when nothing failed |
| `completed.success_ratio` | `sent / (sent + failed)`; `null` when both are 0 |
| `latency.samples` | `sent` rows in the window |
| `latency.p50_ms`, `p95_ms`, `p99_ms` | `percentile_cont` of `sent_at − created_at`; `null` when `samples` is 0 |

Cancelled notifications and retries still waiting are not completions ([K20]).

## Steps

1. **Migration** `00007` with its backfill, and the changed result writes; extend `TestMigrations` (the completion index) and add `TestCompletedAtBackfill`, which migrates to version 6, writes a `sent`, a `failed`, a `queued` and a `cancelled` row, migrates to the end and checks that only the first two got their `completed_at`; assert `completed_at` in the existing delivery, retry and recovery tests.
2. **Snapshot queries** with `as_of` as a parameter.
3. **Response assembly** and the operation; OpenAPI regenerated.
4. **Tests** below.
5. **Cost:** `TestMetricsExplainAnalyze` with a full queue; record the plan and the time.
6. **README:** every field and what it counts; that it is not the R8 proof ([K20]).

## Tests

| Test | Package | Kind | Setup | Asserts |
|---|---|---|---|---|
| `TestMetricsQueueDepth` | `internal/postgres` | Database, fixed `as_of` | Per channel and priority: ready and deferred rows in known numbers | `ready` and `deferred` match exactly |
| `TestMetricsOldestReady` | `internal/postgres` | Database, fixed `as_of` | Ready rows with known `available_at` | Age per priority equals `as_of − min(available_at)`; `null` for a priority with no ready rows |
| `TestMetricsProcessing` | `internal/postgres` | Database, fixed `as_of` | 3 `processing` on `sms`, 0 on `email`, 1 on `push` | `processing` 3, 0, 1 |
| `TestMetricsCompletions` | `internal/postgres` | Database, fixed `as_of` | 8 `sent`, 2 `failed` in the window; 4 `cancelled`; a second fixture with only cancellations | `sent` 8, `failed` 2, `sent_per_second` 8/60, `failed_per_second` 2/60, `success_ratio` 0.8; with only cancellations both rates are `0` and the ratio is `null` |
| `TestMetricsWindowEdges` | `internal/postgres` | Database, fixed `as_of` | Rows completed at `as_of − 60 s`, at `as_of − 60 s + 1 µs`, at `as_of`, and at `as_of + 1 µs` | Counted: the second and third only |
| `TestMetricsLatency` | `internal/postgres` | Database, fixed `as_of` | 100 `sent` rows with known latencies; a channel with none | Percentiles equal those computed in the test from the same values; `samples` 100; the empty channel gives `null` percentiles and 0 samples |
| `TestMetricsAsOfFirstRead` | `internal/postgres` | Database | Time read before and after the call | `as_of` lies between the two |
| `TestMetricsSingleSnapshot` | `internal/postgres` | Database | One ready `sms` row, nothing completed. A test hook runs right after the first statement: another connection marks that row `sent` with `completed_at = as_of − 1 s` and commits | The response shows the state before the change: `ready` 1 and `completed.sent` 0. A read outside one snapshot would show `ready` 0 and `sent` 1 |
| `TestMetricsOperation` | `internal/httpapi` | HTTP + database | Small fixture | `200`; every field present for `sms`, `email` and `push`; the operation is in the OpenAPI document |
| `TestMetricsExplainAnalyze` | `internal/postgres` | Database, measurement | 100,000 rows across statuses | Completes; logs `EXPLAIN ANALYZE` output and total time for each query |

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Snapshot values ([R14]) | `go test -count=1 -v -run '^(TestMetricsQueueDepth\|TestMetricsOldestReady\|TestMetricsProcessing\|TestMetricsCompletions\|TestMetricsWindowEdges\|TestMetricsLatency\|TestMetricsAsOfFirstRead\|TestMetricsSingleSnapshot)$' ./internal/postgres/` | Eight `--- PASS` lines |
| Operation | `go test -count=1 -v -run '^TestMetricsOperation$' ./internal/httpapi/` | `--- PASS: TestMetricsOperation` |
| Cost | `go test -count=1 -v -run '^TestMetricsExplainAnalyze$' ./internal/postgres/` | `--- PASS` and the logged plans and times, copied into the pull request |
| Through the stack | [Stack check](#stack-check) | As described there |

## Stack check

```bash
for i in $(seq 1 5); do
  curl -s -o /dev/null -X POST http://localhost:8080/notifications -H 'Content-Type: application/json' \
    -d '{"recipient":"+905551234567","channel":"sms","content":"metrics"}'
done
sleep 5
curl -s http://localhost:8080/metrics | jq '{as_of, sms: .channels.sms | {processing, completed, latency}}'
```

**Expected observation:** `as_of` is present; for `sms`, `completed.sent` is at least 5, `sent_per_second` is above 0, `failed_per_second` is a number, `success_ratio` is a number, `latency.samples` is at least 5 and the three percentiles are numbers.

## Evidence for the pull request

- Output of every acceptance check and of the stack check.
- The `EXPLAIN ANALYZE` plans and times from `TestMetricsExplainAnalyze`.

## Documentation

- `README.md`: the metrics fields, the window, what is and is not counted.
- `AI_USAGE.md`: slice log row; evidence row for [R14].

[R14]: ../REQUIREMENTS.md#observability
[K8]: ../DECISIONS.md#k8--priority-strict-order-within-each-channel
[K19]: ../DECISIONS.md#k19--openapi-generated-from-the-code-with-huma-on-the-standard-library-router
[K20]: ../DECISIONS.md#k20--metrics-a-json-endpoint-computed-from-postgresql
[K30]: ../DECISIONS.md#k30--health-checks-r16-healthz-for-liveness-readyz-for-readiness
