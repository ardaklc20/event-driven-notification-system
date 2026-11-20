# Slice 8 — Batches

[Delivery plan](../PLAN.md) · Branch `feat/batches` · Core · Depends on: slices 2 and 7

## Goal

`POST /notifications/batch` saves 1 to 1,000 notifications all or nothing, with mixed channels and priorities. `GET /batches/{id}` returns the batch's counts per status and pages through exactly its own notifications.

## What this slice delivers

| ID | Delivered here |
|---|---|
| [R2] | Batch create, 1–1,000 items, `4xx` above that ([K12]) |
| [R4] | Batch status: counts and a complete, paginated list ([K12], [K13]) |
| [R10] | Every item validated with the shared function ([K11]) |
| [R11] | One idempotency key per batch request ([K10]) |
| [R15] | Every item stores the request's correlation ID ([K21]) |

## Preconditions

- Slices 2 and 7 merged.

## Files

| Path | Purpose |
|---|---|
| `migrations/00006_batches.sql` | Schema below |
| `internal/domain/` | Batch request type; validation of every item with its index |
| `internal/postgres/` | Batch transaction; counts; batch page query |
| `internal/service/` | Batch create and status |
| `internal/httpapi/` | Two operations; `MaxBodyBytes` from configuration ([K19]) |
| `internal/config/` | `BATCH_MAX_BODY_BYTES` (proposal), default 16 MiB ([K12]) |
| `api/openapi.yaml` | Regenerated |

## Schema: `00006_batches`

| Object | Content |
|---|---|
| `batches` table | `id` (same type as notification IDs), `item_count integer` not null, `correlation_id text` not null, `created_at timestamptz` not null |
| `notifications.batch_id` | Null for a single create; references `batches(id)` |
| `notifications.batch_position` | Null for a single create; the item's 0-based index in the request; unique together with `batch_id` |
| Index | `notifications (batch_id, created_at DESC, id DESC)` where `batch_id` is not null |
| Checks | `item_count` between 1 and 1,000; `batch_id` and `batch_position` both null or both set |

`batch_position` stores the request order. PostgreSQL documents no order for the rows `RETURNING` gives back, so the `202` response orders its IDs by `batch_position`, not by the order rows come back.

## The batch path, in order ([K10], [K11], [K12])

1. The body is read through `MaxBodyBytes`; a larger body → `413`.
2. Decode; malformed JSON → `400` without echo.
3. Item count 0 or above 1,000 → `422`.
4. Check the structure rules of every item (slice 2); collect every error with `{index, field, message}`; any error → `422`, nothing written, key untouched.
5. Apply defaults to every item; compute the request hash over the normalized items plus the operation `batch`.
6. One transaction: reserve the key (if any) as in slice 2. A repeat returns the stored response. Only the owner checks every item's configurable content limit (a violation rolls back: `422` naming the items' indexes, nothing stored, key free), then inserts the batch row, then all items with one multi-row parameterized `INSERT` (the correlation ID and the batch ID are shared parameters and each item adds five, so 1,000 items use 5,002 parameters, below the protocol's 65,535, [K17]), then stores the `202` response bytes on the key, then commits.
7. The `202` body: `{batch_id, notification_ids: [... in request order]}`.

## Batch status ([K12], [K13])

- `GET /batches/{id}`: `404` for an unknown batch.
- `counts`: one entry per status, zeros included, read in the same snapshot as the page: the batch row, the counts and the page run in one `REPEATABLE READ READ ONLY` transaction.
- `items`: the batch's notifications with the listing order and cursor of slice 7, `limit` default 50. Each notification body now carries `batch_id` (`null` for a single create), also in `GET /notifications/{id}` and the listing.

## Steps

1. **Migration** `00006`; extend `TestMigrations`.
2. **Validation of items** with their index: `TestValidateBatch`.
3. **Batch transaction:** `TestBatchInsertFailureLeavesNothing`.
4. **Operations** and `MaxBodyBytes` from configuration; OpenAPI regenerated.
5. **Status:** counts and paging.
6. **Tests** below.
7. **README:** batch example; the two limits (1,000 items; body size, default 16 MiB, configurable) and why a batch of large items can get `413` with fewer than 1,000 items ([K12]).

## Tests

| Test | Package | Kind | Setup | Asserts |
|---|---|---|---|---|
| `TestValidateBatch` | `internal/domain` | Unit | Items with errors at indexes 0, 7 and 999 | Three errors, each with its index and field |
| `TestBatchSizeLimits` | `internal/httpapi` | HTTP + database | 1,000 valid mixed items; 0 items; 1,001 items | `202` and 1,000 rows each with the batch ID; `422` and nothing written for 0 and for 1,001 |
| `TestBatchInvalidItem` | `internal/httpapi` | HTTP + database | Subtest *structure*: 10 items, the sixth with an unknown channel. Subtest *limit*: 10 items with a key, the third over the `sms` limit | `422` naming index 5 (structure) or index 2 (limit) and the field; no batch row; no notifications; no key |
| `TestBatchBodyLimit` | `internal/httpapi` | HTTP + database | Subtest *default*: a body just over 16 MiB. Subtest *configured*: limit set to 1 MiB, a body just over 1 MiB, and one just under. Every request carries a new key | `413` for both bodies over their limit, and after each: no batch row, no notifications, no key; `202` for the body under 1 MiB |
| `TestBatchInsertFailureLeavesNothing` | `internal/postgres` | Database | The test installs, in its own database, a trigger that raises for one marked item | The transaction fails; no batch row, no notifications, no key |
| `TestBatchResponseOrder` | `internal/httpapi` | HTTP + database | 20 items with distinct contents | `notification_ids[i]` is the row whose `batch_position` is `i` and whose content is item `i`'s |
| `TestBatchIdempotentRepeat` | `internal/httpapi` | HTTP + database | Same key and body twice | Same `202` bytes; same batch ID; no new rows |
| `TestKeyReusedAcrossOperations` | `internal/httpapi` | HTTP + database | Key used for a single create, then for a batch | `422`; nothing written by the batch |
| `TestBatchStatusCounts` | `internal/httpapi` | HTTP + database | Subtest *all*: a batch of 12 with statuses set to 3 `queued`, 2 `processing`, 4 `sent`, 2 `failed`, 1 `cancelled`. Subtest *missing*: a batch of 5 with 2 `queued` and 3 `sent` only | *All*: counts match exactly. *Missing*: `processing`, `failed` and `cancelled` are present in the response with `0` |
| `TestBatchStatusPages` | `internal/httpapi` | HTTP + database | Batch A with 25 items; batch B with 10 items created in between | Walking A with `limit=7` until no `next_cursor` returns each of A's 25 IDs exactly once, every item carries A's batch ID, and none of B's rows appear |
| `TestBatchCorrelationID` | `internal/httpapi` | HTTP + database | Batch with `X-Correlation-ID: batch-1` | Every row stores `batch-1`; `notification_id`s differ |
| `TestGetBatchErrors` | `internal/httpapi` | HTTP + database | One batch | Unknown ID `404`; malformed ID `422`; broken cursor `400`; `limit=201` `422` |
| `TestBatchPageQueryPlan` | `internal/postgres` | Database | Two batches of 1,000 among 5,000 single notifications, statistics updated | The first page and a page after a cursor read `notifications_batch_idx` with no sort |

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Item validation ([R10]) | `go test -count=1 -v -run '^TestValidateBatch$' ./internal/domain/` | `--- PASS: TestValidateBatch` |
| Create ([R2]) | `go test -count=1 -v -run '^(TestBatchSizeLimits\|TestBatchInvalidItem\|TestBatchBodyLimit\|TestBatchResponseOrder)$' ./internal/httpapi/` | Four `--- PASS` lines |
| Atomicity ([K17]) | `go test -count=1 -v -run '^(TestBatchInsertFailureLeavesNothing\|TestBatchPageQueryPlan)$' ./internal/postgres/` | Two `--- PASS` lines |
| Idempotency ([R11]) | `go test -count=1 -v -run '^(TestBatchIdempotentRepeat\|TestKeyReusedAcrossOperations)$' ./internal/httpapi/` | Two `--- PASS` lines |
| Status ([R4]) | `go test -count=1 -v -run '^(TestBatchStatusCounts\|TestBatchStatusPages\|TestGetBatchErrors)$' ./internal/httpapi/` | Three `--- PASS` lines |
| Correlation ID ([R15]) | `go test -count=1 -v -run '^TestBatchCorrelationID$' ./internal/httpapi/` | `--- PASS: TestBatchCorrelationID` |
| Through the stack | [Stack check](#stack-check) | As described there |

## Stack check

```bash
BODY='{"notifications":[
  {"recipient":"+905551234567","channel":"sms","content":"one","priority":"high"},
  {"recipient":"a@example.com","channel":"email","content":"two"},
  {"recipient":"device-token-1","channel":"push","content":"three","priority":"low"}]}'
RESP=$(curl -s -X POST http://localhost:8080/notifications/batch -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: batch-demo-1' -d "$BODY")
BATCH_ID=$(echo "$RESP" | jq -r .batch_id); echo "$RESP" | jq '.notification_ids | length'
for i in $(seq 1 30); do
  SENT=$(curl -s "http://localhost:8080/batches/$BATCH_ID" | jq -r '.counts.sent')
  [ "$SENT" = 3 ] && break
  sleep 1
done
curl -s "http://localhost:8080/batches/$BATCH_ID" | jq '{counts, items: [.items[].channel]}'
```

**Expected observation:** three notification IDs; within 30 seconds `counts.sent` is 3 and the other counts are 0; the items are the three channels of this batch.

## Evidence for the pull request

- Output of every acceptance check and of the stack check.
- Time to write a 1,000-item batch in `TestBatchSizeLimits`, as printed by the test.

## Documentation

- `README.md`: batch create and status examples; the two limits; all-or-nothing behaviour ([K12]).
- `AI_USAGE.md`: slice log row; evidence rows for [R2], [R4], [R10] (batch) and [R11] (batch repeat).

[R2]: ../REQUIREMENTS.md#notification-management-api
[R4]: ../REQUIREMENTS.md#notification-management-api
[R10]: ../REQUIREMENTS.md#processing-engine
[R11]: ../REQUIREMENTS.md#processing-engine
[R15]: ../REQUIREMENTS.md#observability
[K10]: ../DECISIONS.md#k10--idempotency-key-an-optional-idempotency-key-header-one-key-per-create-request
[K11]: ../DECISIONS.md#k11--validation-one-set-of-rules-at-the-api-limits-counted-in-unicode-code-points
[K12]: ../DECISIONS.md#k12--batches-all-or-nothing-with-mixed-channels-and-priorities
[K13]: ../DECISIONS.md#k13--listing-filter-dates-on-created_at-paginate-with-a-created_at-id-cursor
[K17]: ../DECISIONS.md#k17--database-access-pgx-with-plain-sql-in-the-repository
[K19]: ../DECISIONS.md#k19--openapi-generated-from-the-code-with-huma-on-the-standard-library-router
[K21]: ../DECISIONS.md#k21--logging-logslog-json-with-a-correlation-id-stored-on-each-notification
