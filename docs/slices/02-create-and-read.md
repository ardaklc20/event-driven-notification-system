# Slice 2 — Create and read a notification

[Delivery plan](../PLAN.md) · Branch `feat/create-notification` · Core · Depends on: slice 1

## Goal

`POST /notifications` validates a notification, saves it as `queued` and answers `202` with its ID without waiting for delivery. `GET /notifications/{id}` returns it. A repeat with the same `Idempotency-Key` returns the first response byte for byte, even after a content limit has been lowered; the same key with a different request returns `422`; a malformed key returns `400`.

## What this slice delivers

| ID | Delivered here |
|---|---|
| [R1] | Create with `recipient`, `channel`, `content`, `priority`; `202` with ID and `queued` |
| [R3] | Read by ID |
| [R10] | The shared validation function ([K11]) |
| [R11] | The first test of [K3]: API repeats with the same key ([K10]) |
| [R15] | Correlation ID read or generated, returned, stored on the row ([K21]) |
| [D4] | Both operations in the OpenAPI document ([K19]) |

## Preconditions

- Slice 1 merged.

## Files

| Path | Purpose |
|---|---|
| `internal/domain/` | Notification type, channels, priorities, statuses ([K9]); the validation function ([K11]); the request hash |
| `internal/service/` | Create and get, with the interfaces they need ([K23]) |
| `internal/postgres/` | Key reservation, notification insert, stored response, read by ID ([K10], [K17]) |
| `internal/httpapi/` | Operations, middleware (correlation ID, request log, panic recovery) ([K16], [K19], [K21]) |
| `migrations/00002_correlation_and_idempotency.sql` | Schema below |
| `api/openapi.yaml` | Regenerated |

## Schema: `00002_correlation_and_idempotency`

| Table | Column | Type | Rule |
|---|---|---|---|
| `notifications` | `correlation_id` | `text` | Not null ([K21]) |
| `idempotency_keys` | `key` | `text` | Primary key; check constraint `octet_length(key) BETWEEN 1 AND 128` as a second guard behind the header rule |
| | `operation` | `text` | Not null; `create` or `batch` ([K10]) |
| | `request_hash` | `bytea` | Not null |
| | `response_status` | `smallint` | Null until the first response is stored |
| | `response_body` | `bytea` | Null until the first response is stored; the exact bytes sent |
| | `notification_id` | ID type | Null for a batch; set for a single create |
| | `created_at` | `timestamptz` | Not null |

The key row is inserted first, as the reservation, and completed in the same transaction ([K10]); no other transaction can see it before it is complete.

## The `Idempotency-Key` header ([K10])

| Request | Response |
|---|---|
| No header | Accepted; no idempotency |
| Exactly one value, 1–128 bytes, only `A–Z`, `a–z`, `0–9`, `.`, `_`, `-` | Accepted as the key |
| Empty value | `400` |
| Longer than 128 bytes | `400` |
| Any other character (for example a space or `/`) | `400` |
| The header more than once | `400` |

The rule reads every raw value of the header (`Header.Values`), not only the first, so a repeated header is seen. A `400` here writes nothing.

## The create path, in order ([K10], [K11])

1. Check the `Idempotency-Key` header as above → `400`.
2. Decode the JSON body. Malformed JSON → `400`, without the body echoed in the error ([K19]).
3. **Structure rules** of the shared validation: required fields, channel, priority, recipient format, content not empty, no NUL. Violations → `422` with every field error; nothing is written, and the key is not touched.
4. Apply defaults (`priority` → `normal`).
5. Compute the request hash over the normalized request plus the operation name `create`.
6. Begin the transaction. If the request has a key: `INSERT … ON CONFLICT (key) DO NOTHING RETURNING`.
   - **A row comes back** (this request owns the key): check the **configurable limits** (the channel's content length). A violation → roll back, `422` with the field error; nothing is stored and the key stays free. Otherwise insert the notification with the correlation ID, build the `202` response, store its status and bytes on the key row, commit, send those same bytes.
   - **No row** (another transaction owned the key and committed): roll back, then a separate `SELECT` reads `operation`, `request_hash`, `response_status` and `response_body`. Same hash → send the stored bytes with `202` and `Content-Type: application/json` set explicitly ([K19]), whatever the limits are now. Different hash → `422`.
   - **No key:** check the configurable limits (`422` on a violation), insert the notification and commit.
7. The response carries `X-Correlation-ID`.

## Validation rules to implement ([K11])

| Field | Rule |
|---|---|
| `recipient` | Required. `sms`: E.164 (`+`, at most 15 digits). `email`: parses with `net/mail.ParseAddress` and equals the parsed address. `push`: non-empty, no whitespace, at most 512 characters |
| `channel` | Required; `sms`, `email` or `push` |
| `content` | Structure: required, non-empty. Configurable limit, checked only for a new notification: at most 160 (`sms`), 1,000 (`push`), 10,000 (`email`) code points |
| `priority` | Optional; `high`, `normal` or `low`; default `normal` |
| Every text field | No NUL character (U+0000) |

Every rule except the content limit is a structure rule and runs before the key. The content limit runs only when a new notification is about to be saved, so lowering it never turns a replay into `422` ([K10], [K11]).

## Steps

1. **Domain types and validation** in two parts, structure rules and configurable limits, with `TestValidate` written first.
2. **Request hash** over the normalized request and the operation, with `TestRequestHash`.
3. **Migration** `00002`, with `TestMigrations` extended.
4. **Repository:** reserve-then-write transaction and read by ID, with `TestConcurrentRepeatCommit` and `TestConcurrentRepeatRollback`.
5. **Service:** create and get.
6. **Middleware:** correlation ID (valid: at most 64 characters from `A–Z a–z 0–9 - _ .`; otherwise generated), request log line, panic recovery. `TestCorrelationIDHeader`.
7. **Operations:** `POST /notifications` (`DefaultStatus: 202`; replay through the raw-body path with an explicit response schema; the header rule above, `TestIdempotencyKeyHeader`) and `GET /notifications/{id}` (the path ID is declared with `format:"uuid"`, so Huma answers `422` for a malformed ID before the handler runs; `404` for a well-formed ID that does not exist; [K36]). Response fields: `id`, `recipient`, `channel`, `content`, `priority`, `status`, `attempts`, `last_error`, `created_at`, `updated_at`.
8. **OpenAPI** regenerated; `TestOpenAPIUpToDate` passes.
9. **README:** create and get examples, the `Idempotency-Key` header, the replay rule.

## Tests

| Test | Package | Kind | Setup | Asserts |
|---|---|---|---|---|
| `TestValidate` | `internal/domain` | Unit, table | One row per rule, for both parts | Structure: invalid E.164; email with a display name; empty field; NUL character; unknown channel; missing `priority` becomes `normal`. Limits: `sms` 160 passes, 161 fails; Turkish characters counted in code points; a lower configured limit is applied |
| `TestRequestHash` | `internal/domain` | Unit | Pairs of requests | Missing `priority` and `normal` hash the same; the same body under `create` and `batch` hashes differently |
| `TestCreateNotification` | `internal/httpapi` | HTTP + database | Valid request | `202`; `Content-Type: application/json`; body has an ID and `status: queued`; one row |
| `TestGetNotification` | `internal/httpapi` | HTTP + database | One saved notification | `200` with the saved fields; `404` for an unknown UUID; `422` for `not-a-uuid` |
| `TestCreateValidationErrors` | `internal/httpapi` | HTTP + database | Invalid bodies, each with a new key | Malformed JSON `400` without the body in the error; structure violation `422` with field errors; NUL `422`; content over the limit `422`; in every case no row and no key |
| `TestIdempotencyKeyHeader` | `internal/httpapi` | HTTP + database | Valid body with: an empty key; 129 bytes; a space in the key; the header twice; a 128-byte key | `400` for the first four with no row and no key; `202` for the 128-byte key |
| `TestIdempotentRepeat` | `internal/httpapi` | HTTP + database | Same key and body twice | Same ID; one row; second body equals the first byte for byte; the replay has status `202` and `Content-Type: application/json` |
| `TestReplayAfterLimitLowered` | `internal/httpapi` | HTTP + database | API with the `sms` limit at 160: a 120-code-point SMS with key `k1`. Then a second API on the same database with the limit at 100 | Replay with `k1`: the first `202` bytes, one row. The same body with key `k2`: `422`, no row, no key `k2`. Without a key: `422` |
| `TestIdempotencyKeyConflict` | `internal/httpapi` | HTTP + database | Same key, different body | `422`; no new row |
| `TestCreateWithoutKey` | `internal/httpapi` | HTTP + database | Same body twice, no key | Two rows |
| `TestConcurrentRepeatCommit` | `internal/postgres` | Database race | Transaction A reserves the key and holds; B starts with the same key; A commits | B returns no row from its insert, reads A's stored response; one notification |
| `TestConcurrentRepeatRollback` | `internal/postgres` | Database race | A reserves the key and holds; B starts; A rolls back | B's insert succeeds; B writes its notification; one notification, owned by B |
| `TestCorrelationIDHeader` | `internal/httpapi` | HTTP + database | Valid header; invalid header; none | Valid value returned and stored; invalid or missing replaced by a generated one |

`TestReplayAfterSent` needs the worker and is added in slice 3.

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Validation ([R10]) | `go test -count=1 -v -run '^(TestValidate\|TestRequestHash)$' ./internal/domain/` | Two `--- PASS` lines |
| Create and read ([R1], [R3]) | `go test -count=1 -v -run '^(TestCreateNotification\|TestGetNotification\|TestCreateValidationErrors)$' ./internal/httpapi/` | Three `--- PASS` lines |
| Idempotency over HTTP ([R11]) | `go test -count=1 -v -run '^(TestIdempotencyKeyHeader\|TestIdempotentRepeat\|TestIdempotencyKeyConflict\|TestCreateWithoutKey\|TestReplayAfterLimitLowered)$' ./internal/httpapi/` | Five `--- PASS` lines |
| Concurrent repeats ([K10]) | `go test -count=1 -v -run '^(TestConcurrentRepeatCommit\|TestConcurrentRepeatRollback)$' ./internal/postgres/` | Two `--- PASS` lines |
| Correlation ID ([R15]) | `go test -count=1 -v -run '^TestCorrelationIDHeader$' ./internal/httpapi/` | `--- PASS: TestCorrelationIDHeader` |
| Through the stack | [Stack check](#stack-check) | As described there |

## Stack check

```bash
docker compose up -d --build && scripts/wait-health.sh api healthy 60
KEY="demo-key-$(date +%s)"
BODY='{"recipient":"+905551234567","channel":"sms","content":"hello"}'
curl -s -D first.h -o first.json -X POST http://localhost:8080/notifications \
  -H 'Content-Type: application/json' -H "Idempotency-Key: $KEY" -d "$BODY"
ID=$(jq -r .id first.json); echo "id=$ID status=$(jq -r .status first.json)"
curl -s -D second.h -o second.json -X POST http://localhost:8080/notifications \
  -H 'Content-Type: application/json' -H "Idempotency-Key: $KEY" -d "$BODY"
cmp first.json second.json && echo "replay identical"
grep -i '^content-type' first.h second.h
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://localhost:8080/notifications \
  -H 'Content-Type: application/json' -H "Idempotency-Key: $KEY" \
  -d '{"recipient":"+905551234567","channel":"sms","content":"different"}'
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://localhost:8080/notifications \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: has space' -d "$BODY"
curl -s "http://localhost:8080/notifications/$ID" | jq '{id, status}'
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/notifications/not-a-uuid
```

**Expected observation:** `first.h` and `second.h` both start with `HTTP/1.1 202`; `first.h` has an `X-Correlation-ID` header; both show `Content-Type: application/json`; `id=<UUIDv7> status=queued` ([K36]); `replay identical`; the request with a different body prints `422`; the key with a space prints `400`; the `GET` shows the same ID with `status: queued` (no worker yet); the malformed ID prints `422`.

## Failure scenarios

| Scenario | Covered by |
|---|---|
| First transaction commits while a repeat waits | `TestConcurrentRepeatCommit` |
| First transaction rolls back while a repeat waits | `TestConcurrentRepeatRollback` |
| Body that PostgreSQL cannot store (NUL) | `TestCreateValidationErrors` |
| Malformed, oversized or repeated key header | `TestIdempotencyKeyHeader` |
| Content limit lowered after a request was accepted | `TestReplayAfterLimitLowered` |

## Evidence for the pull request

- Output of every acceptance check and of the stack check.
- The JSON shape of the `202` response, as committed in `api/openapi.yaml`.

## Documentation

- `README.md`: create and get examples; the idempotency rule and the key format; the validation limits, that they are application limits, and that they apply to new notifications only ([K10], [K11]).
- `AI_USAGE.md`: slice log row; evidence rows for [R1], [R3], [R10], [R11] (first test) and [R15] (API side).

[R1]: ../REQUIREMENTS.md#notification-management-api
[R3]: ../REQUIREMENTS.md#notification-management-api
[R10]: ../REQUIREMENTS.md#processing-engine
[R11]: ../REQUIREMENTS.md#processing-engine
[R15]: ../REQUIREMENTS.md#observability
[D4]: ../REQUIREMENTS.md#deliverables
[K9]: ../DECISIONS.md#k9--status-model-and-cancellation-five-statuses-and-pending-means-queued
[K10]: ../DECISIONS.md#k10--idempotency-key-an-optional-idempotency-key-header-one-key-per-create-request
[K11]: ../DECISIONS.md#k11--validation-one-set-of-rules-at-the-api-limits-counted-in-unicode-code-points
[K16]: ../DECISIONS.md#k16--http-router-the-standard-librarys-nethttp
[K17]: ../DECISIONS.md#k17--database-access-pgx-with-plain-sql-in-the-repository
[K19]: ../DECISIONS.md#k19--openapi-generated-from-the-code-with-huma-on-the-standard-library-router
[K21]: ../DECISIONS.md#k21--logging-logslog-json-with-a-correlation-id-stored-on-each-notification
[K23]: ../DECISIONS.md#k23--project-layout-small-packages-by-layer-with-multi-table-transactions-inside-postgres
[K36]: ../DECISIONS.md#k36--notification-ids-uuidv7-generated-by-postgresql
