# Slice 5 — Rate limit and priority

[Delivery plan](../PLAN.md) · Branch `feat/rate-limit-priority` · Core · Depends on: slice 4

## Goal

Every claim takes a permit from its channel's shared row in PostgreSQL, work is taken `high → normal → low` within a channel, and **send starts** — not permit times — stay at or below 100 in any one-second window per channel, with two workers and retry traffic.

## What this slice delivers

| ID | Delivered here |
|---|---|
| [R8] | Per-channel limit on send starts: permits 11 ms apart, each valid until a start deadline 100 ms later, measured with a controlled pause and under load ([K5], [K42]) |
| [R9] | Strict priority within a channel, with an explicit order ([K8]) |
| [N1] | A burst is accepted and buffered; nothing is lost |
| [N2] | The limit holds across two workers |

## Preconditions

- Slice 4 merged.
- Send starts are bounded by a permit with a start deadline ([K42]).
- The stack measurement uses the repository's own tools, `cmd/loadgen` and `cmd/ratecount` ([K43]).

## Three times, kept apart ([K5])

| Name | Where it comes from | Used for |
|---|---|---|
| Permit time | The limiter row, database clock | The claim's decision only |
| Send start | Read from the monotonic clock in the start gate, in the same step as the deadline check, at the last point before the HTTP transport; written into the start line as `started_at` ([K21], [K42]) | **R8** |
| Arrival | The mock's records ([K14]) | Reported separately, never as the R8 proof |

A window is half-open: `[t, t + 1 s)`. The count for a channel is the largest number of send starts in any such window, with `t` running over every start time.

## Files

| Path | Purpose |
|---|---|
| `migrations/00004_rate_limit_and_priority.sql` | Limiter rows and the claim index |
| `internal/postgres/` | Claim transaction rebuilt around the permit, its deadline and the priority order; the give-back write ([K42]) |
| `internal/provider/` | The start gate: a wrapper around the client's `http.RoundTripper` that checks the deadline and records the send start in one step ([K42]) |
| `internal/worker/` | The claim loop with a next-try time per channel; passes the deadline to the adapter; gives the attempt back after a refused start; test hooks before the gate and after the claim |
| `internal/config/` | `PERMIT_INTERVAL`, `PERMIT_GRACE` and their validation ([K32], [K42]) |
| `internal/ratecheck/` | Largest count of starts in a half-open one-second window, per channel; used by the tests and by `cmd/ratecount` |
| `cmd/loadgen/` | Sends the R8 load through the API; exits with 1 if any create does not return `202` with an ID ([K43]) |
| `cmd/ratecount/` | Reads the worker's JSON start lines from standard input and prints the window count per channel ([K43]) |

## Schema: `00004_rate_limit_and_priority`

| Object | Content |
|---|---|
| `channel_limits` table | `channel` (primary key), `next_permit_at timestamptz` (not null); one row per channel inserted by the migration |
| Partial index on `notifications` | `(channel, <priority order expression>, available_at, id)` where `status = 'queued'` — the same expression as the claim's `ORDER BY` ([K8]) |

## The claim, final shape ([K5], [K8])

One short transaction:

1. Lock the channel's `channel_limits` row with `FOR UPDATE SKIP LOCKED`. If another worker holds it, return `busy` at once.
2. Read `clock_timestamp()` **once**, together with the row lock; this reading is the permit time. If `next_permit_at` is still after it, return `throttled` with the wait `next_permit_at − permit time`, computed by the database, and no notification.
3. Select one due notification on that channel: `status = 'queued'`, `available_at <= clock_timestamp()`, `attempts < MAX_ATTEMPTS`, ordered by the explicit priority order, then `available_at`, then `id`, `FOR UPDATE SKIP LOCKED`.
4. If none: return `empty`; the permit time is not moved.
5. Otherwise: write the lease and the attempt (slice 3). The permit's start, its deadline and the next permit are all derived from the one reading of step 2, passed back as a parameter: `next_permit_at = permit time + PERMIT_INTERVAL` ([K5]) and deadline `= permit time + PERMIT_GRACE` ([K42]). Only the time left until that deadline is read later, from `clock_timestamp()` at the end of the statement; a later reading can only shorten it. Commit.
6. The worker adds the time left to the monotonic time it read just before the claim, as for the lease ([K3]), and passes that deadline to the provider adapter with the request.

## Claim outcomes and the worker loop

Slice 3's loop knew two outcomes, a claimed row and an empty queue, and waited `IDLE_WAIT` (200 ms) after an empty one. With permits 11 ms apart, most claims on a busy channel now come back without a row only because the next permit is not due yet. Waiting 200 ms after each of those would hold a full channel far below its limit, so the loop tells the outcomes apart and keeps a next-try time per channel:

| Outcome | Meaning | Next try on that channel |
|---|---|---|
| `claimed` | A notification and a permit | At once, while a call slot is free |
| `throttled` | The permit is not due yet | After the wait the claim returned, at most `PERMIT_INTERVAL` |
| `busy` | Another worker holds the limiter row | After `PERMIT_INTERVAL`: the holder is taking or checking the same permit |
| `empty` | The permit is due, but no notification is | After `IDLE_WAIT` |

The loop always tries the channel whose next try comes first, so one channel's wait never delays another channel's claims. Only `empty` waits `IDLE_WAIT`.

## The start gate ([K42])

The deadline check and the counted send start are one step, at the adapter's last point before the request reaches the HTTP transport:

1. The gate wraps the client's `http.RoundTripper`. Its `RoundTrip` reads the monotonic clock and compares it with the deadline carried in the request's context.
2. **Before the deadline:** that reading is the send start. The gate writes the start line with it as `started_at` and calls the inner transport. A pause after this point delays the network send only; it shows in the arrivals, not in the starts.
3. **At or after the deadline:** the gate returns `ErrStartDeadline` without calling the inner transport, so the request certainly never reached it. No start line is written; a `start deadline missed` line is.
4. **Give-back:** on `ErrStartDeadline` the worker runs one `UPDATE` that sets `status = 'queued'`, clears the lease fields and sets `attempts = attempts − 1`, only where the row is `processing`, the token matches and the lease is valid by the database clock. Because it clears the token, it can change the row once. `next_permit_at` is never moved back.
5. Any other error, a crash or an unclear outcome gives nothing back ([K9]).

Lease recovery (slice 4) never touches `channel_limits`.

## Steps

1. **Migration** `00004`; extend `TestMigrations`.
2. **Claim transaction** as above, returning its outcome: `TestPermitKeptWithoutWork`, `TestClaimOutcomes`, `TestChannelsIndependent`, `TestRecoveryWithoutPermit`. **Worker loop** with a next-try time per channel: `TestClaimLoopWaits`.
3. **Priority:** `TestPriorityOrder`, `TestPriorityTieBreak`, `TestClaimQueryPlan`.
4. **Configuration:** `PERMIT_INTERVAL` (default `11ms`) and `PERMIT_GRACE` (default `100ms`). `Validate()` rejects a grace that is not positive and any pair for which `100 × PERMIT_INTERVAL − PERMIT_GRACE` is below 1 second, the condition [K42]'s bound needs ([K32]). Test: `TestPermitConfig`.
5. **Start gate and give-back**, as above: `TestExpiredPermitNoPost`, `TestMissedStartGivesBack`.
6. **Measurement helper and tools:** `internal/ratecheck`, with `TestWindowCount` on hand-made timestamps (99, 100, 101 starts; starts exactly 1 second apart fall in different windows). `cmd/ratecount` keeps the start lines, reads `channel`, `worker_id` and `started_at` from each, and prints one line per channel, `channel=<c> starts=<n> max_window=<m>`, then `workers=<distinct worker_id values>`; it exits with 1 if it finds no start line or cannot parse one. `cmd/loadgen -url <API> -per-channel <n>` sends the creates and prints how many returned `202` ([K43]).
7. **Controlled pause:** `TestSendStartsWithPausedWorker`.
8. **Load test:** `TestRateLimitTwoWorkers`.
9. **In-flight limit:** run the load test with a few `IN_FLIGHT` values and keep the smallest that reaches the limit; record the runs ([K5], "Still open").
10. **Burst:** `TestBurstAccepted`.
11. **Stack measurement**, below.
12. **README:** the limit, how it is measured, and that one passing run is a measurement, not a guarantee ([K5]).

## Tests

| Test | Package | Kind | Setup | Asserts |
|---|---|---|---|---|
| `TestPermitKeptWithoutWork` | `internal/postgres` | Database | Empty channel, permit due | Claim returns `empty`; `next_permit_at` unchanged |
| `TestClaimOutcomes` | `internal/postgres` | Database | Subtest *throttled*: the claim runs with `PERMIT_INTERVAL` set to 1 minute; two due notifications on one channel; both claims share one 5-second context. The first claim takes the permit, so `next_permit_at` is its permit time plus 1 minute, as in normal operation; the second claim follows within the same context. A timeout fails the test explicitly; a successful second claim must run before that permit is due. Subtest *busy*: the channel's limiter row locked by a held transaction | Throttled: both calls succeed within their shared budget; the second returns `throttled` with a wait above 0 and at most `PERMIT_INTERVAL` (1 minute); `next_permit_at` is unchanged by it; the second notification is still `queued` with `attempts` 0. Busy: `busy`, returned at once |
| `TestClaimLoopWaits` | `internal/worker` | Unit, fake claimer and fake clock | The fake claimer returns each outcome in turn; one subtest keeps `sms` `empty` while `email` has work | Next try after `claimed`: at once; after `throttled` with 7 ms: 7 ms; after `busy`: `PERMIT_INTERVAL`; after `empty`: `IDLE_WAIT`. The empty `sms` channel does not delay any `email` claim |
| `TestChannelsIndependent` | `internal/postgres` | Database | A held transaction locks the `sms` limiter row | `email` and `push` claims still succeed |
| `TestRecoveryWithoutPermit` | `internal/postgres` | Database | `sms` limiter row locked by a held transaction; an expired `sms` lease | Recovery still returns the row to `queued` |
| `TestPriorityOrder` | `internal/postgres` | Database | One channel: 50 `low`, then 50 `high`; one claimer, one at a time | All 50 `high` claimed before the first `low`; within a level, by `available_at` |
| `TestPriorityTieBreak` | `internal/postgres` | Database | One channel: one ready row per level; one `high` with `available_at` in the future; two ready `normal` rows with the same `available_at` | Claim order: ready `high`, the lower-ID `normal`, the other `normal`, `low`; the future `high` is not claimed until its time |
| `TestClaimQueryPlan` | `internal/postgres` | Database | 10,000 queued rows, statistics updated | `EXPLAIN` of the claim names the partial index |
| `TestWindowCount` | `internal/ratecheck` | Unit | Hand-made start times | Correct largest count; window edges half-open |
| `TestPermitConfig` | `internal/config` | Unit | Variables through `Options.Environment` | `11ms` with `100ms` accepted; `10ms` with `100ms` rejected (`1000 − 100 < 1000` ms); a grace of `0` rejected |
| `TestExpiredPermitNoPost` | `internal/worker` | Worker + database + `httptest` | A hook stops the worker inside the start gate, before its check; the test releases it 200 ms later, after the deadline | The provider receives no request; no start line and one `start deadline missed` line; the row is `queued` with its lease cleared and `attempts` back to its value before the claim; `next_permit_at` was not moved back; a later claim sends it, and the provider receives it once |
| `TestMissedStartGivesBack` | `internal/postgres` | Database | A claimed row; subtests: the give-back with the valid token; the same give-back run a second time; a lease that has expired; a row recovered and claimed again under a new token | Valid token: one row changed, `attempts` reduced by one. Second run, expired lease, other token: 0 rows changed and `attempts` unchanged |
| `TestSendStartsWithPausedWorker` | `internal/worker` | Two workers + database + `httptest` | One channel full; worker A's hook pauses for 300 ms right after its claim commits; worker B runs normally | Both workers record starts; A's paused claim misses its deadline and sends nothing; that notification is sent later, once; no half-open one-second window holds more than 100 starts |
| `TestRateLimitTwoWorkers` | `internal/worker` | Two workers + database + `httptest` | 1,000 queued per channel; the provider answers `503` only to the **first attempt** of every tenth notification, `202` otherwise | Every notification ends `sent` within 30 seconds (about 11 at the permit spacing; waiting `IDLE_WAIT` after every permit that is not yet due would take minutes); retries appear among the starts; both workers started sends, since any worker may take any channel's permit ([K5], [K42]); per channel, no window holds more than 100; in the busiest second, the three channels together exceed 100 (they do not share one permit) |
| `TestBurstAccepted` | `internal/e2e` | API + worker + `httptest` | 3,000 creates as fast as the test can send, across the three channels | Every create returns `202`; 3,000 rows; every notification ends `sent` within a bounded wait |

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Claim and priority ([R9]) | `go test -count=1 -v -run '^(TestPermitKeptWithoutWork\|TestClaimOutcomes\|TestChannelsIndependent\|TestRecoveryWithoutPermit\|TestPriorityOrder\|TestPriorityTieBreak\|TestClaimQueryPlan)$' ./internal/postgres/` | Seven `--- PASS` lines |
| Claim loop | `go test -count=1 -v -run '^TestClaimLoopWaits$' ./internal/worker/` | `--- PASS: TestClaimLoopWaits` |
| Window counting | `go test -count=1 -v -run '^TestWindowCount$' ./internal/ratecheck/` | `--- PASS: TestWindowCount` |
| Permit settings ([K42]) | `go test -count=1 -v -run '^TestPermitConfig$' ./internal/config/` | `--- PASS: TestPermitConfig` |
| Give-back ([K42]) | `go test -count=1 -v -run '^TestMissedStartGivesBack$' ./internal/postgres/` | `--- PASS: TestMissedStartGivesBack` |
| Send starts ([R8], [N2]) | `go test -count=1 -v -run '^(TestExpiredPermitNoPost\|TestSendStartsWithPausedWorker\|TestRateLimitTwoWorkers)$' ./internal/worker/` | Three `--- PASS` lines; the test log prints the largest window per channel |
| Burst ([N1]) | `go test -count=1 -v -run '^TestBurstAccepted$' ./internal/e2e/` | `--- PASS: TestBurstAccepted` |
| Stack ([R8]) | [Stack measurement](#stack-measurement-with-two-workers) | As described there |

## Stack measurement with two workers

The measurement runs in its own Compose project, so its database, its worker logs and the mock's counters hold this run only; nothing from earlier runs is counted.

1. Free the host ports with `docker compose stop` (main stack), then start the run:

   ```bash
   P="nsys-r8-$(date +%s)"
   PROVIDER_URL=http://mockprovider:8081/ PROVIDER_PROFILE=standard MOCK_DELAY=0s \
     docker compose -p "$P" up -d --build --scale worker=2
   scripts/wait-health.sh -p "$P" api healthy 60
   ```

   The provider and the mock's delay are set on the command itself: a separate project does not isolate the shell's environment, and Compose would otherwise take `PROVIDER_URL` from it and could send this load to a real URL, or take a `MOCK_DELAY` that stretches the run and adds timeouts and retries.

2. Queue 1,000 notifications per channel through the API:

   ```bash
   go run ./cmd/loadgen -url http://127.0.0.1:8080 -per-channel 1000; echo "loadgen exit=$?"
   ```
3. Wait at most 120 seconds until nothing is `queued` or `processing`:

   ```bash
   for i in $(seq 1 120); do
     LEFT=$(docker compose -p "$P" exec -T postgres psql -U "${POSTGRES_USER:-postgres}" -tAc \
       "select count(*) from notifications where status in ('queued','processing')")
     [ "$LEFT" = 0 ] && break
     sleep 1
   done
   echo "left=$LEFT"
   docker compose -p "$P" exec -T postgres psql -U "${POSTGRES_USER:-postgres}" -tAc \
     "select channel, status, count(*) from notifications group by 1, 2 order by 1, 2"
   ```

4. Count the send starts with the same window code as the tests:

   ```bash
   docker compose -p "$P" logs --no-log-prefix worker | go run ./cmd/ratecount; echo "ratecount exit=$?"
   docker compose -p "$P" logs --no-log-prefix worker | grep -c 'start deadline missed'
   ```
5. Collect the arrivals: `curl -s --max-time 5 http://localhost:8081/stats | jq .`.
6. Clean-up: `docker compose -p "$P" down -v`, then `docker compose start` for the main stack.

**Expected observation:** `loadgen exit=0` with 3,000 creates answered `202`; `left=0` within 30 seconds of the loop's start (at about 91 starts per second per channel, 1,000 per channel take about 11 seconds from the first create; the number of seconds the loop took goes into the pull request), and the count query prints exactly three lines, `email|sent|1000`, `push|sent|1000` and `sms|sent|1000`: `left=0` alone would also hold if notifications had failed. `ratecount exit=0` with `starts=1000` and `max_window` at most 100 on every channel line, and `workers=2`; the arrivals at the mock add up to 3,000. The number of missed deadlines is reported; a missed deadline produces no start, so it does not change these counts. The mock answers every request with `202`, so this run has no retries; retry traffic is covered by `TestRateLimitTwoWorkers`. The arrival maximum per channel is reported next to it, separately. Both numbers go into the pull request; neither is presented as a guarantee on its own ([K5]). The bound itself comes from [K42]'s spacing and deadline; this run shows it holding under load and does not replace that argument.

## Failure scenarios

| Scenario | Covered by |
|---|---|
| A worker pauses after taking its permit | `TestSendStartsWithPausedWorker` |
| A worker reaches the send point after its deadline | `TestExpiredPermitNoPost` |
| A give-back after recovery, after the lease expired, or a second time | `TestMissedStartGivesBack` |
| Another worker holds a channel's limiter row | `TestChannelsIndependent`, `TestClaimOutcomes` |
| A permit not yet due, or an empty channel, slowing the others | `TestClaimLoopWaits`, the 30-second bound in `TestRateLimitTwoWorkers` |
| Retries compete with first attempts | `TestRateLimitTwoWorkers` |
| Recovery while the channel row is locked | `TestRecoveryWithoutPermit` |
| A burst far above the send rate | `TestBurstAccepted` |

## Evidence for the pull request

- Output of every acceptance check, including the largest window per channel from each test run.
- The in-flight runs: `IN_FLIGHT` value, largest window, total time, for each run.
- The stack measurement: send-start and arrival maxima per channel, side by side, with the number of workers and notifications.
- The number of `start deadline missed` lines in the stack measurement.

## Documentation

- `README.md`: the rate limit, the priority rule and its starvation cost ([K8]), the measurement and its limits.
- `AI_USAGE.md`: slice log row; evidence rows for [R8], [R9], [N1] and [N2], each with the numbers actually observed.

[R8]: ../REQUIREMENTS.md#processing-engine
[R9]: ../REQUIREMENTS.md#processing-engine
[N1]: ../REQUIREMENTS.md#non-functional-requirements-from-the-case-text
[N2]: ../REQUIREMENTS.md#non-functional-requirements-from-the-case-text
[K3]: ../DECISIONS.md#k3--delivery-guarantee-a-durable-queue-lease-recovery-and-bounded-retries
[K5]: ../DECISIONS.md#k5--rate-limiter-one-shared-permit-row-per-channel-in-postgresql
[K8]: ../DECISIONS.md#k8--priority-strict-order-within-each-channel
[K9]: ../DECISIONS.md#k9--status-model-and-cancellation-five-statuses-and-pending-means-queued
[K14]: ../DECISIONS.md#k14--provider-a-small-go-mock-by-default-webhooksite-as-a-separate-profile
[K21]: ../DECISIONS.md#k21--logging-logslog-json-with-a-correlation-id-stored-on-each-notification
[K32]: ../DECISIONS.md#k32--configuration-typed-environment-variables-with-caarlos0env
[K42]: ../DECISIONS.md#k42--send-starts-a-permit-with-a-start-deadline
[K43]: ../DECISIONS.md#k43--measurement-tools-small-go-programs-in-the-repository
