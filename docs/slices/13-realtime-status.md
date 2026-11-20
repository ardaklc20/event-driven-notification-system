# Slice 13 — Real-time status updates

[Delivery plan](../PLAN.md) · Branch `feat/realtime-status` · Optional ([B4]) · Depends on: slices 6, 8 and 12

## Goal

A WebSocket client that follows one notification or one batch receives the current state first and then best-effort updates as statuses change. Nothing is promised beyond that: a client that needs the exact state reconnects for a new snapshot or reads it with `GET` ([K25]).

## What this slice delivers

| ID | Delivered here |
|---|---|
| [B4] | `GET /ws/notifications/{id}` and `GET /ws/batches/{id}`: a snapshot, then best-effort updates ([K24], [K25]) |

## Preconditions

- Slices 6, 8 and 12 merged. Scheduled notifications come before this slice ([K26], [K24]).
- The leak check uses `goleak` v1.3.0 ([K45]); requests while the listener is down get `503` before the upgrade ([K46]); the stack check uses `cmd/wswatch` ([K43]).

## Files

| Path | Purpose |
|---|---|
| `migrations/00010_status_version.sql` | Schema below |
| `internal/postgres/` | The listener on a hijacked pool connection; the snapshot reads |
| `internal/realtime/` | Subscribers, their buffers, the version filter and the listener generation; no SQL ([K23]) |
| `internal/httpapi/` | The two WebSocket handlers on the `ServeMux` ([K16]) |
| `internal/config/` | WebSocket settings ([K32]) |
| `cmd/api/` | Starts the listener; registers the WebSocket close function with `RegisterOnShutdown` |
| `cmd/wswatch/` | Follows one WebSocket and prints each message as one line ([K43]) |
| `go.mod` | `go.uber.org/goleak` v1.3.0, used by tests only ([K45]) |
| `README.md` | Message format; OpenAPI does not describe WebSockets ([K25]) |

## Schema: `00010_status_version`

| Object | Content |
|---|---|
| `notifications.version` | `bigint` not null, default 0 |
| Trigger before update | When `status` changes (`IS DISTINCT FROM`), sets `version = OLD.version + 1` |
| Trigger after update | When `status` changed, calls `pg_notify('notification_status', payload)` |
| Payload | JSON: `notification_id`, `batch_id`, `status`, `attempts`, `version`, `updated_at`; far below the 8,000-byte limit ([K24]) |

Every path that changes a status passes through the triggers: the claim, a result, a retry, a cancel and lease recovery ([K24]). Row locks serialize updates to one row, so `version` only grows per notification.

## How a subscription works ([K24], [K25])

1. The handler checks that the notification or batch exists; an unknown ID gets `404` and no upgrade. While no listener generation is live, it answers `503` with `Retry-After: 1` and does not upgrade ([K46]).
2. The connection is upgraded; `CloseRead` starts, so ping, pong and close frames are answered.
3. The subscriber is registered with the hub under the current listener generation, and its events are buffered from this moment. If the listener dropped during the upgrade, the registration is refused and the connection is closed at once, before any snapshot ([K46]).
4. The snapshot is read from the database: one notification, or every notification of the batch, each with its `version`.
5. The snapshot is sent as the first message.
6. The buffer, then every later event, is passed through the version filter: for each event, compare with the last version sent **for that notification** (its snapshot version first). Send it only when it is higher, then record it.
7. One writer per connection sends from a bounded queue, each write with a time limit. A full queue closes that connection.

For a batch, the filter keeps one version per notification of the batch. Versions of different notifications are never compared with each other.

## The listener

- One connection taken out of the pool with `Hijack`; it runs `LISTEN notification_status` and then waits in `WaitForNotification`.
- It never opens a transaction, so it cannot hold back cleanup of PostgreSQL's notification queue ([K24]).
- **Generation ([K46]):** the listener's generation number grows each time it connects. A registration and a drop are handled under the same lock in the hub, so a subscription can never be registered under a generation that has already ended.
- If the connection drops: under that lock the generation ends and every subscription of it is closed, because any of them may have missed an event; the listener reconnects with a growing pause; clients that reconnect receive a fresh snapshot.
- Requests that arrive while no generation is live get `503` with `Retry-After: 1` before the upgrade; REST and `/readyz` are not affected ([K46]).

## Settings ([K32])

| Variable (proposal) | Default | Rule in `Validate()` |
|---|---|---|
| `WS_SEND_QUEUE` | `16` messages | At least 1 |
| `WS_WRITE_TIMEOUT` | `5s` | Above 0 |
| `WS_CLOSE_TIMEOUT` | `5s` | Below `SHUTDOWN_TIMEOUT` ([K6]) |

## Steps

1. **Migration** `00010` and its triggers; extend `TestMigrations`.
2. **Version and notify tests** against every status path.
3. **Listener** in `internal/postgres`.
4. **Hub and version filter** in `internal/realtime`, tested without a network.
5. **Handlers** and the writer; `404` before the upgrade; `CloseRead`.
6. **Shutdown:** `RegisterOnShutdown` sends close status 1001 ("going away") to every connection and waits at most `WS_CLOSE_TIMEOUT` ([K25]).
7. **Leak check** with `goleak` v1.3.0 ([K45]): each WebSocket test takes `goleak.IgnoreCurrent()` before it opens any of its resources and calls `goleak.VerifyNone` with it after the listener, the sockets and the server are closed. These tests do not call `t.Parallel()`, so another test's goroutines cannot appear.
8. **`cmd/wswatch`** ([K43]): flags `-url` and `-for`; prints each text message as one line; exits with 0 after `-for`, and with 1 if the connection fails or the server closes it with an error status.
9. **README:** endpoints, messages with an example, the best-effort contract, reconnecting for a fresh snapshot.

## Tests

Database tests use the Compose PostgreSQL image ([K22]); WebSocket tests use `httptest.Server` and the library's client.

| Test | Package | Kind | Setup | Asserts |
|---|---|---|---|---|
| `TestStatusVersionIncrements` | `internal/postgres` | Database | One row taken through claim, a retry, claim, result; another through cancel; another through lease recovery | `version` grows by exactly 1 on each status change, and not on updates that leave `status` as it was |
| `TestNotifyOnCommitOnly` | `internal/postgres` | Database | A listening connection; one status change committed, one rolled back | Only the committed change arrives |
| `TestNotifyPayload` | `internal/postgres` | Database | A batch item changes status | The payload holds all six fields with the row's values; its size is below 8,000 bytes |
| `TestListenerOutsideTransaction` | `internal/postgres` | Database | The listener is waiting | Its backend in `pg_stat_activity` has a null `xact_start` |
| `TestVersionFilter` | `internal/realtime` | Unit, table | Snapshot versions and event sequences | Only events above the last sent version of the same notification pass |
| `TestWebSocketUnknownID` | `internal/httpapi` | HTTP | Both endpoints with an unknown ID | `404`; no upgrade |
| `TestWebSocketSnapshotThenUpdate` | `internal/httpapi` | WebSocket + database | Connect; then the test changes the status | First message is the snapshot; the next is the update with a higher `version` |
| `TestSnapshotNoGap` | `internal/httpapi` | WebSocket + database | A test hook commits a status change after registration and before the snapshot read ends | The client ends with the newest status, in the snapshot or as the next message |
| `TestOlderEventsDropped` | `internal/httpapi` | WebSocket + database | Buffered events with versions at or below the snapshot's | None of them is sent |
| `TestBatchWebSocket` | `internal/httpapi` | WebSocket + database | A batch of 3; one item changes | The snapshot lists all 3; the change arrives as one update |
| `TestBatchVersionPerNotification` | `internal/httpapi` | WebSocket + database | Batch with A at version 5 and B at version 1 in the snapshot; then events B v2 and A v4 | B v2 is sent although 2 < 5; A v4 is dropped |
| `TestListenerDropClosesSockets` | `internal/httpapi` | WebSocket + database | Two clients; the test ends the listener's backend with `pg_terminate_backend` | Both connections receive a close frame; after the listener is back, a reconnecting client gets a fresh snapshot |
| `TestWebSocketWhileListenerDown` | `internal/httpapi` | WebSocket + database | Listener stopped; a client connects to each endpoint | `503` with `Retry-After: 1`; no `101`, so no upgrade ([K46]) |
| `TestListenerDropDuringUpgrade` | `internal/httpapi` | WebSocket + database | A hook stops the handler after the upgrade and before registration; the test ends the listener's backend, waits until the drop is handled, then releases the hook | The registration is refused; the client receives a close frame and no snapshot; the hub holds no subscription of the ended generation; after the listener is back, a new client receives a snapshot ([K46]) |
| `TestSlowClientDisconnected` | `internal/httpapi` | WebSocket + database | Two clients on one batch; one stops reading; 100 status changes | The slow one is closed when its queue fills; the other receives every update, each within 1 second of its commit |
| `TestControlFrames` | `internal/httpapi` | WebSocket | Client pings | The ping returns without error within 1 second |
| `TestWebSocketShutdown` | `internal/httpapi` | WebSocket | Three open connections; the server shuts down | Each receives close status 1001 before shutdown returns; shutdown returns within `WS_CLOSE_TIMEOUT` plus 1 second |
| `TestWebSocketNoLeak` | `internal/httpapi` | WebSocket | `goleak.IgnoreCurrent()` first; then open and close 50 connections, and close the listener and the server | `goleak.VerifyNone` reports no goroutine ([K45]) |

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Events at the source ([K24]) | `go test -count=1 -v -run '^(TestStatusVersionIncrements\|TestNotifyOnCommitOnly\|TestNotifyPayload\|TestListenerOutsideTransaction)$' ./internal/postgres/` | Four `--- PASS` lines |
| Version filter | `go test -count=1 -v -run '^TestVersionFilter$' ./internal/realtime/` | `--- PASS: TestVersionFilter` |
| Contract ([B4]) | `go test -count=1 -v -run '^(TestWebSocketUnknownID\|TestWebSocketSnapshotThenUpdate\|TestSnapshotNoGap\|TestOlderEventsDropped\|TestBatchWebSocket\|TestBatchVersionPerNotification)$' ./internal/httpapi/` | Six `--- PASS` lines |
| Failure scenarios ([K25], [K45], [K46]) | `go test -count=1 -v -run '^(TestListenerDropClosesSockets\|TestWebSocketWhileListenerDown\|TestListenerDropDuringUpgrade\|TestSlowClientDisconnected\|TestControlFrames\|TestWebSocketShutdown\|TestWebSocketNoLeak)$' ./internal/httpapi/` | Seven `--- PASS` lines |
| Through the stack | [Stack check](#stack-check) | As described there |

## Stack check

The client is the repository's `cmd/wswatch` ([K43]): it prints each message as one line and exits after the given time.

```bash
ID=$(curl -s -X POST http://localhost:8080/notifications -H 'Content-Type: application/json' \
  -d "{\"recipient\":\"+905551234567\",\"channel\":\"sms\",\"content\":\"watch\",\"scheduled_at\":\"$(date -u -v+15S +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '+15 seconds' +%Y-%m-%dT%H:%M:%SZ)\"}" | jq -r .id)
go run ./cmd/wswatch -url "ws://localhost:8080/ws/notifications/$ID" -for 30s | tee ws.out
jq -r '[.status, .version] | @tsv' ws.out
```

**Expected observation:** the first line is the snapshot with `queued`; later lines show `processing`, then `sent`, with `version` growing on every line.

## Review items to apply before this slice starts

Found in the plan review; this note is updated with them in the slice's own pull request.

1. **Slow client:** 100 small frames can sit in TCP buffers without filling the application queue. `TestSlowClientDisconnected` blocks the slow connection's writer with a test hook, so the queue really fills.
2. **Ping:** `TestControlFrames` runs a reader on the client side (for example `CloseRead`); the library's `Ping` receives the pong only while the connection is being read.
3. **Shutdown:** the function registered with `RegisterOnShutdown` only starts closing (it sends status 1001); it must not wait. The bounded wait of `WS_CLOSE_TIMEOUT` runs in `cmd/api`'s shutdown sequence, as [K25] already states. Step 6 and `TestWebSocketShutdown` are rewritten to match.

## Evidence for the pull request

- Output of every acceptance check and of the stack check.

## Documentation

- `README.md`: the two endpoints, the message format with an example, the best-effort contract, what a client does after a disconnect; the bonus listed as done only after this pull request merges.
- `AI_USAGE.md`: slice log row; evidence row for [B4].

[B4]: ../REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[K6]: ../DECISIONS.md#k6--process-layout-the-api-and-the-worker-run-as-separate-processes-from-one-go-module
[K16]: ../DECISIONS.md#k16--http-router-the-standard-librarys-nethttp
[K22]: ../DECISIONS.md#k22--test-database-testcontainers-with-the-same-postgresql-image-as-compose
[K23]: ../DECISIONS.md#k23--project-layout-small-packages-by-layer-with-multi-table-transactions-inside-postgres
[K24]: ../DECISIONS.md#k24--real-time-status-events-b4-postgresql-listennotify
[K25]: ../DECISIONS.md#k25--websocket-endpoint-b4-coderwebsocket-one-notification-or-batch-per-connection
[K26]: ../DECISIONS.md#k26--scheduled-notifications-b2-an-optional-absolute-scheduled_at
[K32]: ../DECISIONS.md#k32--configuration-typed-environment-variables-with-caarlos0env
[K43]: ../DECISIONS.md#k43--measurement-tools-small-go-programs-in-the-repository
[K45]: ../DECISIONS.md#k45--goroutine-leak-check-goleak
[K46]: ../DECISIONS.md#k46--websocket-requests-while-the-listener-is-down-503
