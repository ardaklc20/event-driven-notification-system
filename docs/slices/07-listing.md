# Slice 7 — Listing

[Delivery plan](../PLAN.md) · Branch `feat/listing` · Core · Depends on: slice 2

## Goal

`GET /notifications` lists notifications newest first, filtered by `status`, `channel` and a half-open `created_at` range, with a cursor that new notifications cannot shift.

## What this slice delivers

| ID | Delivered here |
|---|---|
| [R6] | Combined filters and cursor pagination ([K13]) |

## Preconditions

- Slice 2 merged.
- The page size follows [K44]: 50 by default, 1 to 200, `422` outside.

## Files

| Path | Purpose |
|---|---|
| `migrations/00005_listing_index.sql` | Index for the listing order |
| `internal/postgres/` | Listing query with optional filters ([K17]) |
| `internal/service/` | Listing use case, cursor encoding |
| `internal/httpapi/` | Operation, parameter validation |
| `api/openapi.yaml` | Regenerated |

## Schema: `00005_listing_index`

- Index on `notifications (created_at DESC, id DESC)` ([K13]).
- No filter index yet; one on `(channel, status, created_at, id)` is added only if `EXPLAIN` shows a filtered query needs it, in this slice's pull request with the plan before and after.

## The operation ([K13])

| Parameter | Rule |
|---|---|
| `status` | Optional; one of the five statuses |
| `channel` | Optional; `sms`, `email` or `push` |
| `from`, `to` | Optional RFC 3339 timestamps; `from` included, `to` excluded |
| `limit` | Optional; Huma tags `default:"50" minimum:"1" maximum:"200"`, so the rule is in the OpenAPI document; an empty `?limit=` gets the default; `0` or `201` gets `422` ([K44]) |
| `cursor` | Optional; opaque; produced by the previous page |

- Filters combine with AND.
- Order: `created_at DESC, id DESC`.
- The query reads `limit + 1` rows to learn whether another page exists; `next_cursor` is built from the last returned row's `created_at` at full precision and its `id`, and is absent on the last page.
- The next page reads rows below the cursor: `(created_at, id) < (cursor values)`.
- A cursor that does not decode, or decodes to invalid values → `400`.

## Steps

1. **Migration** `00005`.
2. **Query** with optional filters as numbered parameters; no string concatenation of values ([K17]).
3. **Cursor** encode and decode, with `TestCursorRoundTrip`.
4. **Operation** and parameter validation; OpenAPI regenerated.
5. **Tests** below, in the repository and through HTTP.
6. **README:** listing example, how to follow `next_cursor`, what the cursor does and does not guarantee ([K13]).

## Tests

| Test | Package | Kind | Setup | Asserts |
|---|---|---|---|---|
| `TestCursorRoundTrip` | `internal/service` | Unit | Cursors with microsecond timestamps | Decode of encode returns the same values; garbage fails |
| `TestListFilters` | `internal/postgres` | Database | 30 rows across every status and channel and three days | Each filter alone, and all combined, return exactly the matching rows, newest first |
| `TestListDateRange` | `internal/postgres` | Database | Rows exactly at `from` and exactly at `to` | The row at `from` included; the row at `to` excluded |
| `TestListCursorWithInserts` | `internal/postgres` | Database | 25 rows; read pages of 7; between page reads, insert 5 rows whose `created_at` is set explicitly later than the first page's newest row | Each of the original 25 appears exactly once across the pages; none of the new 5 appear on the pages after the first |
| `TestListCursorTieBreak` | `internal/postgres` | Database | 10 rows with the same `created_at`, placed so that a page of 4 ends in the middle of them | Walking the pages returns all 10 once, in descending ID order across the page boundary; no ID repeated or skipped |
| `TestListFiltersHTTP` | `internal/httpapi` | HTTP + database | Same fixture as `TestListFilters` | The same results through `GET /notifications` with query parameters |
| `TestListMalformedCursor` | `internal/httpapi` | HTTP | `cursor=not-a-cursor` | `400` |
| `TestListLimit` | `internal/httpapi` | HTTP + database | 250 rows | 50 rows without `limit` and with `?limit=`; 200 with `limit=200`; `422` with `limit=0` and with `limit=201`, naming `limit` ([K44]) |
| `TestListQueryPlan` | `internal/postgres` | Database | 10,000 rows, statistics updated | `EXPLAIN` of the unfiltered first page names the listing index |

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Cursor | `go test -count=1 -v -run '^TestCursorRoundTrip$' ./internal/service/` | `--- PASS: TestCursorRoundTrip` |
| Repository ([R6]) | `go test -count=1 -v -run '^(TestListFilters\|TestListDateRange\|TestListCursorWithInserts\|TestListCursorTieBreak\|TestListQueryPlan)$' ./internal/postgres/` | Five `--- PASS` lines |
| HTTP ([R6]) | `go test -count=1 -v -run '^(TestListFiltersHTTP\|TestListMalformedCursor\|TestListLimit)$' ./internal/httpapi/` | Three `--- PASS` lines |
| Through the stack | [Stack check](#stack-check) | As described there |

## Stack check

```bash
for c in sms sms email; do
  curl -s -o /dev/null -X POST http://localhost:8080/notifications -H 'Content-Type: application/json' \
    -d "{\"recipient\":\"$( [ $c = sms ] && echo +905551234567 || echo a@example.com )\",\"channel\":\"$c\",\"content\":\"list\"}"
done
PAGE=$(curl -s 'http://localhost:8080/notifications?channel=sms&limit=1')
echo "$PAGE" | jq '{count: (.items | length), next_cursor}'
CURSOR=$(echo "$PAGE" | jq -r .next_cursor)
curl -s -G http://localhost:8080/notifications --data-urlencode channel=sms --data-urlencode limit=1 \
  --data-urlencode "cursor=$CURSOR" | jq '{count: (.items | length), channels: [.items[].channel]}'
curl -s -o /dev/null -w '%{http_code}\n' 'http://localhost:8080/notifications?cursor=broken'
curl -s -o /dev/null -w '%{http_code}\n' 'http://localhost:8080/notifications?limit=201'
```

**Expected observation:** the first page has one item and a `next_cursor`; the second page has one `sms` item; the broken cursor prints `400`; `limit=201` prints `422` ([K44]).

## Evidence for the pull request

- Output of every acceptance check and of the stack check.
- The `EXPLAIN` output of the first page with and without filters, and whether the filter index was added.

## Documentation

- `README.md`: listing example, parameters, cursor guarantees and limits ([K13]).
- `AI_USAGE.md`: slice log row; evidence row for [R6].

[R6]: ../REQUIREMENTS.md#notification-management-api
[K13]: ../DECISIONS.md#k13--listing-filter-dates-on-created_at-paginate-with-a-created_at-id-cursor
[K17]: ../DECISIONS.md#k17--database-access-pgx-with-plain-sql-in-the-repository
[K44]: ../DECISIONS.md#k44--page-size-50-by-default-1-to-200-422-outside
