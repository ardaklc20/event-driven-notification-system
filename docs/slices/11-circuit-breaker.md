# Slice 11 — Circuit breaker

[Delivery plan](../PLAN.md) · Branch `feat/circuit-breaker` · Optional ([B1]) · Depends on: slices 5 and 10

## Goal

During a provider outage, one breaker shared by all workers stops new claims on every channel without spending waiting notifications' attempts. After the cooldown, exactly one probe goes out; only that probe, while its lease is valid, can close or reopen the breaker; any other probe outcome frees the slot at once; and a late or stale answer never moves it.

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
| `docker-compose.yml` | The `worker` service passes the three breaker settings through from the shell, as it does `PROVIDER_URL` |

## Schema: `00008_provider_breaker`

Table `provider_breaker`, one row (check constraint on a fixed key):

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
| Probe authority | A probe's answer moves the breaker only while `probe_token` is still this probe's **and** its probe lease is valid by the database clock (`probe_expires_at > clock_timestamp()`). This covers closing, reopening and every cooldown change |
| Probe answers `202` | With authority: closed, counters reset, cooldown back to the first value, slot freed |
| Probe answers with an outage failure | With authority: reopened, cooldown doubled up to the cap, slot freed |
| Probe answers with anything else | A permanent error, a terminal protocol error ([K41]) or a `429` under a profile that states no shared quota. The notification is written as [K7] and [K41] say: a permanent or terminal error → `failed`; a `429` under `webhook_site_free` → `failed`, because [K7] makes it permanent there; a `429` under `standard` → back to `queued` with its `Retry-After` while attempts are left, `failed` when none are. In the same transaction, which locks the breaker row first, the slot is freed if `probe_token` is still this probe's; `generation`, the counters and `cooldown` stay as they are, and the next claim becomes the probe |
| Probe misses its start deadline ([K42]) | The start gate refused it, so no POST went out. The give-back write frees the slot in the same transaction if `probe_token` is still this probe's; it locks the breaker row before the notification row and leaves `generation`, the counters and `cooldown` unchanged |
| Stale answer | A probe whose token is no longer current or whose probe lease has expired, or any result from an older generation, writes its notification under [K3]'s lease rules and **never** moves the breaker, whether it is `202` or `503` |
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

Compose passes each one to the `worker` service with these defaults (`BREAKER_COOLDOWN: ${BREAKER_COOLDOWN:-30s}` and so on), so a value set on a `docker compose up` command reaches the container.

## Steps

1. **Migration** `00008`, with the single row.
2. **Claim:** read the breaker before the limiter row; the probe slot path.
3. **Result writes:** generation, token and probe-lease checks; lock order; the slot freed by every probe outcome that does not move the breaker, and by the give-back after a missed start deadline.
4. **Classification** of outage failures; the shared-quota flag, set only by a test profile.
5. **Settings**, their Compose pass-through, and `Validate()` (threshold at least 1; cooldown at most the maximum).
6. **Log lines** ([K21]): `breaker opened`, `breaker reopened` and `breaker closed`, each with `generation` and `cooldown`.
7. **Tests** below.
8. **README:** what happens during a provider outage; that a failure on one channel pauses all three ([K34]).

All worker scenarios run with two workers and an `httptest` provider whose answers the test controls, with threshold 3 and cooldown 1 second.

## Failure scenarios

| Test | Package | Setup | Expected observation |
|---|---|---|---|
| `TestBreakerOpens` | `internal/worker` | Provider answers `503`; 30 queued across three channels | After 3 failures no worker claims on any channel; the waiting rows' `attempts` stay the same while open |
| `TestBreakerSingleProbe` | `internal/worker` | Breaker open; cooldown ends; both workers try to claim | Exactly one POST reaches the provider |
| `TestBreakerRecovers` | `internal/worker` | Provider answers `202` from now on | The probe closes the breaker; every waiting row ends `sent` |
| `TestBreakerProbeFails` | `internal/worker` | The probe gets `503` twice in a row | Reopens each time; the cooldown doubles and never exceeds the maximum |
| `TestProbeSlotFreeWithoutWork` | `internal/postgres` | Breaker open, cooldown over, no queued rows | The claim rolls back; `probe_token` stays null |
| `TestProbePermanentError` | `internal/worker` | Subtests: the probe gets `400` (permanent); the probe gets `404` (terminal, [K41]) | That row `failed`; slot freed; counters and cooldown unchanged; the next claim becomes the probe |
| `TestProbeTooManyRequestsFreesSlot` | `internal/worker` | `standard` profile; queued rows on all three channels; the probe gets `429` with `Retry-After: 5` | That row is `queued` with `available_at` at least 5 seconds later; the slot is free in the same transaction; within 1 second, long before the probe's lease would end, the other worker's claim becomes the new probe and its POST reaches the provider; counters and cooldown unchanged |
| `TestProbeMissedStartFreesSlot` | `internal/worker` | A hook stops the probe inside the start gate; the test releases it after its deadline ([K42]) | No POST from that probe; the slot is free at once; the next claim takes a new probe; the provider receives exactly one POST, from the new probe |
| `TestLateAcceptedResult` | `internal/worker` | A call started before the opening answers `202` after it, lease still valid | Its row `sent`; the breaker stays open; `consecutive_failures` unchanged |
| `TestStaleProbeResultNoTransition` | `internal/worker` | Subtests `202` and `503`: probe A's lease expires; probe B takes the slot; then A answers | Neither subtest closes, reopens or changes the cooldown; A's row is not written (its lease is gone); B's answer is the one that moves the breaker |
| `TestExpiredProbeLeaseNoTransition` | `internal/worker` | Subtests `202` and `503`: probe A's lease expires and no new probe has taken the slot yet; then A answers | Neither subtest closes, reopens or changes the cooldown; A's row is not written |
| `TestOldGenerationFailureIgnored` | `internal/postgres` | A failure result carrying the previous generation | `consecutive_failures` unchanged |
| `TestCrashedProbe` | `internal/worker` | The probe's worker stops without a result | After `probe_expires_at`, another worker takes the probe |
| `TestRecoveryWhileOpen` | `internal/postgres` | Breaker open; an expired lease | Recovery returns the row to `queued` without touching the breaker |
| `TestRetryAfterDelaysOnlyItsNotification` | `internal/worker` | `standard` profile; one row gets `429` with `Retry-After: 5` | Only that row waits; others keep being claimed; breaker closed |
| `TestSharedQuotaOpensBreaker` | `internal/worker` | Test profile stating a shared quota; `429` with `Retry-After: 5`, then one with `Retry-After: 1` | The breaker opens until the first time; the second does not shorten it |
| `TestBreakerConcurrentClaimsAndResults` | `internal/postgres` | Many concurrent claims and result writes while the breaker opens and closes | No PostgreSQL deadlock error (SQLSTATE `40P01`) |

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Worker scenarios ([B1]) | `go test -count=1 -v -run '^(TestBreakerOpens\|TestBreakerSingleProbe\|TestBreakerRecovers\|TestBreakerProbeFails\|TestProbePermanentError\|TestProbeTooManyRequestsFreesSlot\|TestProbeMissedStartFreesSlot\|TestLateAcceptedResult\|TestStaleProbeResultNoTransition\|TestExpiredProbeLeaseNoTransition\|TestCrashedProbe\|TestRetryAfterDelaysOnlyItsNotification\|TestSharedQuotaOpensBreaker)$' ./internal/worker/` | Thirteen `--- PASS` lines, every subtest passing |
| Repository scenarios | `go test -count=1 -v -run '^(TestProbeSlotFreeWithoutWork\|TestOldGenerationFailureIgnored\|TestRecoveryWhileOpen\|TestBreakerConcurrentClaimsAndResults)$' ./internal/postgres/` | Four `--- PASS` lines |
| Through the stack | [Stack check](#stack-check) | As described there |

## Stack check

The check runs in its own Compose project with an empty database, so the breaker's row and the counted notifications hold this run only; the breaker's state lives in PostgreSQL, so recreating a worker alone would not reset it. The cooldown is short and capped low, so a failing probe shows within seconds and the restore never waits for a long, doubled cooldown. The provider and the mock's delay are set on every `up` command; a slow mock would make the restore look like another outage.

```bash
docker compose stop          # the main stack; frees the host ports, keeps its volume
P="nsys-breaker-$(date +%s)"
export BREAKER_COOLDOWN=2s BREAKER_MAX_COOLDOWN=4s
PROVIDER_URL=http://mockprovider:9/ PROVIDER_PROFILE=standard MOCK_DELAY=0s \
  docker compose -p "$P" up -d --build
scripts/wait-health.sh -p "$P" api healthy 60
for i in $(seq 1 10); do
  curl -s -o /dev/null -X POST http://localhost:8080/notifications -H 'Content-Type: application/json' \
    -d '{"recipient":"+905551234567","channel":"sms","content":"outage"}'
done
for i in $(seq 1 30); do       # at most 30 s: the breaker opens and a probe fails
  docker compose -p "$P" logs --no-log-prefix worker | grep -q '"msg":"breaker reopened"' && break
  sleep 1
done
docker compose -p "$P" logs --no-log-prefix worker | grep -cE '"msg":"breaker (opened|reopened)"'
docker compose -p "$P" exec -T postgres psql -U notifications -d notifications -tAc \
  "select status, count(*) from notifications group by 1 order by 1"
PROVIDER_URL=http://mockprovider:8081/ PROVIDER_PROFILE=standard MOCK_DELAY=0s \
  docker compose -p "$P" up -d --force-recreate worker      # restore: the mock answers 202
for i in $(seq 1 30); do       # at most 30 s: one probe closes the breaker, the queue drains
  LEFT=$(docker compose -p "$P" exec -T postgres psql -U notifications -d notifications -tAc \
    "select count(*) from notifications where status <> 'sent'")
  [ "$LEFT" = 0 ] && break
  sleep 1
done
echo "left=$LEFT"
docker compose -p "$P" logs --no-log-prefix worker | grep -c '"msg":"breaker closed"'
docker compose -p "$P" down -v; unset BREAKER_COOLDOWN BREAKER_MAX_COOLDOWN
docker compose start         # the main stack
```

**Expected observation:** the first loop ends early with at least one `breaker reopened` line, and the count of opening lines is at least 2; while the provider is down, the status query shows only `queued` (a probe in flight may show as `processing`) and no `failed`; after the restore `left=0`, and the closing count is at least 1.

## Evidence for the pull request

- Output of every acceptance check and of the stack check.

## Documentation

- `README.md`: provider outage behaviour and settings; the bonus listed as done only after this pull request merges.
- `AI_USAGE.md`: slice log row; evidence row for [B1].

[B1]: ../REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[K3]: ../DECISIONS.md#k3--delivery-guarantee-a-durable-queue-lease-recovery-and-bounded-retries
[K7]: ../DECISIONS.md#k7--retry-policy-retry-by-error-type-exponential-backoff-with-jitter-five-attempts-in-total
[K9]: ../DECISIONS.md#k9--status-model-and-cancellation-five-statuses-and-pending-means-queued
[K21]: ../DECISIONS.md#k21--logging-logslog-json-with-a-correlation-id-stored-on-each-notification
[K32]: ../DECISIONS.md#k32--configuration-typed-environment-variables-with-caarlos0env
[K34]: ../DECISIONS.md#k34--failure-handling-b1-one-circuit-breaker-for-the-provider-shared-by-all-workers
[K41]: ../DECISIONS.md#k41--responses-k7-does-not-list-terminal-protocol-errors
[K42]: ../DECISIONS.md#k42--send-starts-a-permit-with-a-start-deadline
