# Slice 11 — Circuit breaker

[Delivery plan](../PLAN.md) · Branch `feat/circuit-breaker` · Optional ([B1]) · Depends on: slices 5 and 10

## Goal

During a provider outage, one breaker shared by all workers stops new claims on every channel without spending waiting notifications' attempts. After the cooldown, exactly one probe goes out; only that probe can close or reopen the breaker; and a late or stale answer never moves it.

## What this slice delivers

| ID | Delivered here |
|---|---|
| [B1] | Failure handling for a shared provider outage ([K34]) |

## Preconditions

- Slices 5 and 10 merged.

## Files

| Path | Purpose |
|---|---|
| `migrations/00008_provider_breaker.sql` | Schema below |
| `internal/postgres/` | Breaker read and probe slot in the claim; breaker update in result writes |
| `internal/provider/` | Outage classification; the shared-quota profile flag |
| `internal/config/` | Threshold and cooldown settings ([K32]) |

## Schema: `00008_provider_breaker`

One row (check constraint on a fixed key):

| Column | Type | Meaning |
|---|---|---|
| `generation` | `bigint` | Increases each time the breaker opens |
| `consecutive_failures` | `integer` | Outage failures in a row, current generation only |
| `open_until` | `timestamptz` | Null when closed; end of the pause when open |
| `cooldown` | `interval` | Current cooldown; doubles on each reopening, capped |
| `probe_token` | `uuid` | Null unless a probe holds the slot |
| `probe_expires_at` | `timestamptz` | The probe's lease end |

## Rules ([K34])

| Situation | Rule |
|---|---|
| Closed | Claims run as before; the claim reads the breaker without locking it |
| Outage failure | Timeouts, network errors, `408` and the retried `5xx`, from the current generation, increment `consecutive_failures`; reaching the threshold opens the breaker, increments `generation`, sets `open_until = clock_timestamp() + cooldown` |
| Not an outage failure | `429` (any profile) and permanent errors |
| Success while closed | From the current generation: `consecutive_failures = 0` |
| Open, before `open_until` | The claim grants no permit; nothing is claimed on any channel |
| Open, after `open_until` | The claim takes the probe slot with a conditional update in the same transaction as the permit and the claim; no notification or no permit → rollback; slot stays free |
| Probe answers `202` | Only with the current `probe_token`: closed, counters reset, cooldown back to the first value |
| Probe answers with an outage failure | Only with the current `probe_token`: reopened, cooldown doubled up to the cap |
| Probe answers with a permanent error | That notification `failed` ([K7]); slot freed; no change to the counters |
| Stale answer | A probe whose token is no longer current, or any result from an older generation, writes its notification under [K3]'s lease rules and **never** moves the breaker, whether it is `202` or `503` |
| Probe worker crashes | The slot expires at `probe_expires_at`, the probe's lease end |
| `429` with `Retry-After` | Delays only its notification, unless the profile states a shared quota; then it opens the breaker until that time and a later, shorter value never shortens the pause |
| Lease recovery | Unaffected by the breaker ([K9]) |
| Lock order | A transaction that locks the breaker row locks it before the limiter row and before any notification row |

## Settings ([K32])

| Variable (proposal) | Default |
|---|---|
| `BREAKER_THRESHOLD` | `5` |
| `BREAKER_COOLDOWN` | `30s` |
| `BREAKER_MAX_COOLDOWN` | `5m` |

## Steps

1. **Migration** `00008`, with the single row.
2. **Claim:** read the breaker before the limiter row; the probe slot path.
3. **Result writes:** generation and token checks; lock order.
4. **Classification** of outage failures; the shared-quota flag, set only by a test profile.
5. **Settings** and `Validate()` (threshold at least 1; cooldown at most the maximum).
6. **Tests** below.
7. **README:** what happens during a provider outage; that a failure on one channel pauses all three ([K34]).

All worker scenarios run with two workers and an `httptest` provider whose answers the test controls, with threshold 3 and cooldown 1 second.

## Failure scenarios

| Test | Package | Setup | Expected observation |
|---|---|---|---|
| `TestBreakerOpens` | `internal/worker` | Provider answers `503`; 30 queued across three channels | After 3 failures no worker claims on any channel; the waiting rows' `attempts` stay the same while open |
| `TestBreakerSingleProbe` | `internal/worker` | Breaker open; cooldown ends; both workers try to claim | Exactly one POST reaches the provider |
| `TestBreakerRecovers` | `internal/worker` | Provider answers `202` from now on | The probe closes the breaker; every waiting row ends `sent` |
| `TestBreakerProbeFails` | `internal/worker` | The probe gets `503` twice in a row | Reopens each time; the cooldown doubles and never exceeds the maximum |
| `TestProbeSlotFreeWithoutWork` | `internal/postgres` | Breaker open, cooldown over, no queued rows | The claim rolls back; `probe_token` stays null |
| `TestProbePermanentError` | `internal/worker` | The probe gets `400` | That row `failed`; slot freed; counters unchanged; the next claim becomes the probe |
| `TestLateAcceptedResult` | `internal/worker` | A call started before the opening answers `202` after it, lease still valid | Its row `sent`; the breaker stays open; `consecutive_failures` unchanged |
| `TestStaleProbeResultNoTransition` | `internal/worker` | Subtests `202` and `503`: probe A's lease expires; probe B takes the slot; then A answers | Neither subtest closes, reopens or changes the cooldown; A's row is not written (its lease is gone); B's answer is the one that moves the breaker |
| `TestOldGenerationFailureIgnored` | `internal/postgres` | A failure result carrying the previous generation | `consecutive_failures` unchanged |
| `TestCrashedProbe` | `internal/worker` | The probe's worker stops without a result | After `probe_expires_at`, another worker takes the probe |
| `TestRecoveryWhileOpen` | `internal/postgres` | Breaker open; an expired lease | Recovery returns the row to `queued` without touching the breaker |
| `TestRetryAfterDelaysOnlyItsNotification` | `internal/worker` | `standard` profile; one row gets `429` with `Retry-After: 5` | Only that row waits; others keep being claimed; breaker closed |
| `TestSharedQuotaOpensBreaker` | `internal/worker` | Test profile stating a shared quota; `429` with `Retry-After: 5`, then one with `Retry-After: 1` | The breaker opens until the first time; the second does not shorten it |
| `TestBreakerConcurrentClaimsAndResults` | `internal/postgres` | Many concurrent claims and result writes while the breaker opens and closes | No PostgreSQL deadlock error (SQLSTATE `40P01`) |

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Worker scenarios ([B1]) | `go test -count=1 -v -run '^(TestBreakerOpens\|TestBreakerSingleProbe\|TestBreakerRecovers\|TestBreakerProbeFails\|TestProbePermanentError\|TestLateAcceptedResult\|TestStaleProbeResultNoTransition\|TestCrashedProbe\|TestRetryAfterDelaysOnlyItsNotification\|TestSharedQuotaOpensBreaker)$' ./internal/worker/` | Ten `--- PASS` lines, every subtest passing |
| Repository scenarios | `go test -count=1 -v -run '^(TestProbeSlotFreeWithoutWork\|TestOldGenerationFailureIgnored\|TestRecoveryWhileOpen\|TestBreakerConcurrentClaimsAndResults)$' ./internal/postgres/` | Four `--- PASS` lines |
| Through the stack | [Stack check](#stack-check) | As described there |

## Stack check

1. Point the worker at an address where nothing listens: `PROVIDER_URL=http://mockprovider:9/ docker compose up -d --force-recreate worker`.
2. Create 10 notifications; wait 15 seconds.
3. `docker compose logs --no-log-prefix worker | grep -c breaker` and `curl -s http://localhost:8080/metrics | jq '.channels.sms.queue'`.
4. Restore: `docker compose up -d --force-recreate worker`; wait at most 60 seconds, polling until no row is `queued`.

**Expected observation:** the log shows the breaker opening and probes failing; the waiting notifications stay `queued` with low `attempts`; after the restore one probe closes the breaker and all ten end `sent`.

## Review items to apply before this slice starts

Found in the plan review; this note is updated with them in the slice's own pull request.

1. **Probe authority ([K34], chosen):** a probe's answer moves the breaker only with the current `probe_token` **and** a probe lease still valid by the database clock (`probe_expires_at > clock_timestamp()`). The rule covers closing, reopening and every cooldown change. K34's text is updated with it in this pull request. New test: probe A's lease expires and no new probe has taken the slot yet; A then answers `202` (subtest) or `503` (subtest); neither moves the breaker.
2. **Stack check timing:** the default cooldown is 30 seconds, so the 15-second wait above cannot show a failing probe. Run the demo with a short cooldown (for example `BREAKER_COOLDOWN=2s`) and poll the breaker's state with a bounded loop instead of a fixed wait.
3. **A probe that misses its start deadline ([K42]):** the start gate refuses it, so no POST went out. The give-back write then also frees the probe slot in the same transaction, but only if the breaker's `probe_token` is still this probe's token. It locks the breaker row before the notification row (the lock order above) and leaves `generation`, the counters and `cooldown` unchanged. Otherwise every other probe would wait until `probe_expires_at` for a probe that was never sent. K34's text gets the matching sentence in this pull request. New test: a probe is stopped inside the start gate past its deadline and released; the slot is free at once, the next claim takes a new probe, and the provider receives exactly one POST, from the new probe.

## Evidence for the pull request

- Output of every acceptance check and of the stack check.

## Documentation

- `README.md`: provider outage behaviour and settings; the bonus listed as done only after this pull request merges.
- `AI_USAGE.md`: slice log row; evidence row for [B1].

[B1]: ../REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[K3]: ../DECISIONS.md#k3--delivery-guarantee-a-durable-queue-lease-recovery-and-bounded-retries
[K7]: ../DECISIONS.md#k7--retry-policy-retry-by-error-type-exponential-backoff-with-jitter-five-attempts-in-total
[K9]: ../DECISIONS.md#k9--status-model-and-cancellation-five-statuses-and-pending-means-queued
[K32]: ../DECISIONS.md#k32--configuration-typed-environment-variables-with-caarlos0env
[K34]: ../DECISIONS.md#k34--failure-handling-b1-one-circuit-breaker-for-the-provider-shared-by-all-workers
[K42]: ../DECISIONS.md#k42--send-starts-a-permit-with-a-start-deadline
