# Delivery Plan

This plan turns [`REQUIREMENTS.md`](REQUIREMENTS.md) (R1–R16, N1–N3, B1–B6, D1–D6, A1–A2) and [`DECISIONS.md`](DECISIONS.md) (K0–K47) into sixteen slices that are built, verified and merged one at a time. It adds only what the records lack: the order, the dependencies, the steps and the acceptance checks. **Where this plan disagrees with the records, the records win**, and the plan is corrected in the pull request that finds the difference ([K35]).

This file holds what spans slices. Each slice has its own note under [`slices/`](slices/) with its preconditions, files, schema changes, steps, tests, acceptance checks, failure scenarios and the evidence its pull request records.

## Conventions

- **One slice, one branch, one pull request** ([K4]), merged with a merge commit after the [slice gate](#slice-gate) passes.
- **Go checks.** A targeted check runs `go test -count=1 -v -run '^TestName$' ./internal/<package>/`. It passes only when it exits with 0 **and** prints `--- PASS: TestName`. `ok … [no tests to run]` means the test does not exist, and counts as a failure.
- **The whole suite.** `go test -count=1 ./...` passes when every line starts with `ok` or with `?` (a package with `[no test files]`), no line contains `FAIL`, and none contains `(cached)` ([K22]).
- **Stack checks** run from the repository root against `docker compose`. Every wait has an upper bound: a loop with a fixed number of tries, never an open-ended wait. Health waits go through `scripts/wait-health.sh` (slice 1), which exits with 1 on timeout, so a failed wait fails the check. Notification IDs are kept in shell variables (`ID`, `BATCH_ID`), never pasted by hand.
- **Isolated Compose projects.** A check that deletes volumes, talks to a real external provider or measures counts runs under its own Compose project with a unique, lowercase name (`-p` or `COMPOSE_PROJECT_NAME`) and an empty database, and cleans up only that project. A different clone folder alone is not enough, because a fixed project name in the environment or the Compose file would make two folders share one project.
- **Release checks** (slices 10 and 16) run on a fresh clone of the pull request's head commit and record its SHA. The branch must already contain `main`, and CI must have tested that same commit: the workflow checks out the pull request's head commit instead of GitHub's default merge commit and logs it as `tested-sha` ([K29]).
- **Checks that must stop on failure** are scripts, not pasted blocks: `scripts/release-check.sh` (slice 10) and `scripts/webhook-check.sh` (slice 4) run with `set -euo pipefail`, print `FAIL: <step>` and exit with 1 at the first failed step, and print `PASS` only when every step passed.
- **The provider is pinned on the command.** Compose takes `PROVIDER_URL`, `PROVIDER_PROFILE` and `MOCK_DELAY` from the shell's environment, and a separate project does not change that. A check that must use the mock sets `PROVIDER_URL=http://mockprovider:8081/ PROVIDER_PROFILE=standard MOCK_DELAY=0s` on its `up` command; only a check that needs a slow mock sets another delay there.
- **Ports and tools** ([K39], [K43]): pasted commands use the default host ports, the API at `http://localhost:8080` and the mock provider at `http://localhost:8081`, bound to `127.0.0.1`. The check scripts read `API_PORT`, `MOCK_PORT`, `POSTGRES_PORT` and `BIND_ADDRESS` like Compose does. The R8 load and window count use `cmd/loadgen` and `cmd/ratecount`, the WebSocket check `cmd/wswatch`; `curl`, `jq` and `gh` are needed for the evidence commands only. Environment variable names that no decision fixes are proposals, and the README lists the final ones ([K32]).
- **Evidence.** A plan or slice note states each command and its expected observation *before* the slice is built. The pull request records what was actually run and what was observed, measurements included ([K35]). `AI_USAGE.md` lists only checks that were actually run, with the observed result.
- **Status** in the delivery order is `planned` until the slice's pull request merges; that pull request changes it to `done` with its link.

## Slice gate

Every slice passes all of these before its pull request is merged.

| # | Check | Command | Expected observation |
|---|---|---|---|
| 1 | Formatting and lint ([K28]) | `golangci-lint run` | Exit code 0 and `0 issues.` |
| 2 | Whole suite ([K22], [D6]) | `go test -count=1 ./...` | Every line starts with `ok` or `?`; no `FAIL`; no `(cached)` |
| 3 | Race detector ([K29]) | `go test -count=1 -race ./...` | Same as row 2, and no `WARNING: DATA RACE` |
| 4 | The slice's own checks | Each command in the slice note's *Acceptance checks* | Each expected observation, with every targeted test printing its `--- PASS` line |
| 5 | CI ([K29], from slice 1 on) | `gh pr checks` | Every job of the CI workflow shows `pass` |
| 6 | OpenAPI ([K19], from slice 1 on) | `go test -count=1 -v -run '^TestOpenAPIUpToDate$' ./internal/httpapi/` | `--- PASS: TestOpenAPIUpToDate` |
| 7 | Evidence ([K35]) | — (pull request description) | The commands of rows 1–6 with their observed output, and the measurements the slice note asks for |
| 8 | Records | — (diff of the pull request) | `README.md` covers what the slice adds ([D2]); `AI_USAGE.md` has the slice's log row and requirement → evidence rows for checks actually run; this plan's status column is updated |

## Scope

- **Core:** [R1]–[R16] and [N1]–[N3], built first, in slices 1–10. Slice 10 is the core delivery: on its own, it must satisfy [D1]–[D6].
- **Optional (bonus):** [B1]–[B6], built after the core in slices 11–15, except [B6], which [K29] puts in the first working slice.
- **Deliverables:** [D1]–[D6], checked on a clean clone in slice 10, and again in slice 16 for what the bonuses add.
- **Assumptions:** [A1] (no authentication) is documented as a limitation; [A2] (one provider URL, the channel in the body) shapes slice 3.
- **Process-only decisions:** [K0] (local container runtime), [K4] (Git workflow) and [K35] (this plan) govern how the work is done, not what is built.

## Delivery order

| # | Slice | Scope | Requirements | Decisions | Depends on | Branch | Status |
|---|---|---|---|---|---|---|---|
| 1 | [Walking skeleton](slices/01-walking-skeleton.md) | Core + B6 | R16, D3, D4, D5, D6, B6 | K1, K6, K8, K16, K17, K18, K19, K21, K22, K23, K28, K29, K30, K31, K32, K36, K37, K38, K39 | — | `feat/skeleton` | done ([#8](https://github.com/ardaklc20/event-driven-notification-system/pull/8)) |
| 2 | [Create and read a notification](slices/02-create-and-read.md) | Core | R1, R3, R10, R11, R15, D4 | K2, K9, K10, K11, K16, K17, K19, K21, K36 | 1 | `feat/create-notification` | done ([#9](https://github.com/ardaklc20/event-driven-notification-system/pull/9)) |
| 3 | [Delivery to the provider](slices/03-delivery.md) | Core | R7, R11, R12, R13, R15, N2, A2 | K2, K3, K6, K9, K14, K15, K21, K23, K31, K32, K39 | 2 | `feat/delivery` | done ([#10](https://github.com/ardaklc20/event-driven-notification-system/pull/10)) |
| 4 | [Retries and crash recovery](slices/04-retries-and-recovery.md) | Core | R11, R12, R13, N3 | K3, K7, K9, K14, K15, K21, K31, K32, K39, K40, K41 | 3 | `feat/retries-recovery` | done ([#11](https://github.com/ardaklc20/event-driven-notification-system/pull/11)) |
| 5 | [Rate limit and priority](slices/05-rate-limit-and-priority.md) | Core | R8, R9, N1, N2 | K2, K5, K6, K8, K14, K21, K32, K42, K43 | 4 | `feat/rate-limit-priority` | done ([#12](https://github.com/ardaklc20/event-driven-notification-system/pull/12)) |
| 6 | [Cancellation](slices/06-cancellation.md) | Core | R5 | K5, K9, K22 | 5 | `feat/cancel` | done ([#13](https://github.com/ardaklc20/event-driven-notification-system/pull/13)) |
| 7 | [Listing](slices/07-listing.md) | Core | R6 | K13, K17, K19, K44 | 2 | `feat/listing` | done ([#14](https://github.com/ardaklc20/event-driven-notification-system/pull/14)) |
| 8 | [Batches](slices/08-batches.md) | Core | R2, R4, R10, R11, R15 | K10, K11, K12, K13, K17, K19, K21 | 2, 7 | `feat/batches` | done ([#15](https://github.com/ardaklc20/event-driven-notification-system/pull/15)) |
| 9 | [Metrics](slices/09-metrics.md) | Core | R14 | K8, K19, K20, K30 | 4 | `feat/metrics` | done ([#16](https://github.com/ardaklc20/event-driven-notification-system/pull/16)) |
| 10 | [Core release check](slices/10-core-release-check.md) | Core | D1–D6, A1, R13 | K3, K4, K10, K11, K14, K18, K19, K29, K31, K32, K39 | 1–9 | `docs/core-readme` | done ([#17](https://github.com/ardaklc20/event-driven-notification-system/pull/17)) |
| 11 | [Circuit breaker](slices/11-circuit-breaker.md) | Optional | B1 | K3, K5, K7, K9, K21, K34, K41, K42 | 5, 10 | `feat/circuit-breaker` | done ([#18](https://github.com/ardaklc20/event-driven-notification-system/pull/18)) |
| 12 | [Scheduled notifications](slices/12-scheduled-notifications.md) | Optional | B2 | K5, K8, K9, K10, K12, K20, K26, K42 | 6, 8, 9, 10 | `feat/scheduled` | done ([#19](https://github.com/ardaklc20/event-driven-notification-system/pull/19)) |
| 13 | [Real-time status updates](slices/13-realtime-status.md) | Optional | B4 | K6, K9, K12, K16, K18, K24, K25, K43, K45, K46 | 6, 8, 12 | `feat/realtime-status` | done ([#20](https://github.com/ardaklc20/event-driven-notification-system/pull/20)) |
| 14 | [Templates](slices/14-templates.md) | Optional | B3 | K10, K11, K12, K13, K33, K36, K44, K47 | 8, 10 | `feat/templates` | planned |
| 15 | [Distributed tracing](slices/15-distributed-tracing.md) | Optional | B5 | K21, K27, K42 | 10 | `feat/tracing` | planned |
| 16 | [Final release check](slices/16-final-release-check.md) | Core + optional | D1–D6, completed bonuses | K4, K19, K29, K31 | 10 and every bonus slice marked `done` | `docs/final-readme` | planned |

### Why this order

1. **Recorded constraints.** Lint and CI come with the first working slice ([K28], [K29]); scheduled notifications come before the WebSocket updates ([K26], [K24]); tracing comes after the core flow and its tests pass ([K27]); the health endpoint ([R16]) is part of the core ([K30]).
2. **Dependencies.** The worker needs saved notifications (2 → 3). The R8 proof needs retry traffic ([K5]), so retries come before the rate limit (4 → 5). The cancel races are tested against the final claim transaction, which holds the permit, and against retry writes ([K5], [K9]), so cancellation follows both (5 → 6). Batch status pages with the listing cursor ([K12], [K13]), so listing comes before batches (7 → 8). Metrics need completed, failed and retried notifications ([K20]), so they follow slice 4.
3. **Delivery risk.** Once their dependencies are met, the slices hardest to change later come first: the claim, lease and limiter transactions (3–6) before listing, batches and metrics (7–9). Among the bonuses, the circuit breaker (11) changes the claim transaction, so it comes first.
4. **End-to-end verifiability.** From slice 3 on, `docker compose up` runs the whole path from the API through the worker to the mock provider, and every later slice extends a system that already runs.

### What blocks which slice

Only the dependencies in the delivery order. Every question that would have blocked a slice was settled before implementation ([K36]–[K47]; see [Settled before implementation](#settled-before-implementation)), so slice 1 can start once this plan is merged.

## Slices at a glance

| # | Goal in one sentence |
|---|---|
| 1 | `docker compose up` starts PostgreSQL, the migrations and a healthy API; every pull request runs lint, tests, the OpenAPI check and the image build in CI. |
| 2 | `POST /notifications` validates and queues a notification and answers `202` at once; `GET` reads it; a repeat with the same key returns the first response byte for byte. |
| 3 | A worker process claims queued notifications under a lease and delivers them to the provider; through the mock in Compose a notification reaches `sent`. |
| 4 | Temporary failures are retried with backoff, permanent ones stop, crashed workers' notifications are taken back, and a stale worker can never write. |
| 5 | Every claim takes a permit from its channel's shared row, work is taken `high → normal → low`, and send starts stay at or below 100 per second per channel. |
| 6 | A cancel stops a `queued` notification for good, and races with claims and result writes always end with one winner. |
| 7 | `GET /notifications` filters and pages newest first with a stable cursor. |
| 8 | A batch of 1–1,000 notifications is saved all or nothing, and its status is read with counts and complete paging. |
| 9 | `GET /metrics` reports queue depth, `processing`, 60-second rates and latency from one snapshot. |
| 10 | The core, on a clean clone, starts with one command, passes its tests, matches its OpenAPI document and has real webhook.site evidence. |
| 11 | One shared circuit breaker pauses every channel during a provider outage and reopens with a single probe. |
| 12 | A notification with `scheduled_at` is not claimed before that time. |
| 13 | WebSocket clients get a snapshot and then best-effort status updates. |
| 14 | Immutable stored templates render once at the API under the K11 rules. |
| 15 | API requests, worker attempts and provider calls produce linked traces. |
| 16 | The finished project, with the bonuses that were completed, passes the clean-clone checks again. |

## Coverage

| ID | Slices | Planned verification |
|---|---|---|
| [R1] | 2 | `TestCreateNotification`, stack check in [02](slices/02-create-and-read.md#stack-check) |
| [R2] | 8 | `TestBatchSizeLimits` |
| [R3] | 2 | `TestGetNotification` |
| [R4] | 8 | `TestBatchStatusPages`, `TestBatchStatusCounts` |
| [R5] | 6 | `TestCancelResponses`, `TestCancelClaimRace`, `TestCancelResultRace`, `TestCancelClaimRaceRepeated`, `TestNoPostAfterCancel` |
| [R6] | 7 | `TestListFilters`, `TestListFiltersHTTP`, `TestListCursorWithInserts`, `TestListCursorTieBreak`, `TestListLimit` |
| [R7] | 3 | `TestCreateWhileProviderBlocked` |
| [R8] | 5 | `TestExpiredPermitNoPost`, `TestMissedStartGivesBack`, `TestSendStartsWithPausedWorker`, `TestRateLimitTwoWorkers`, stack measurement in [05](slices/05-rate-limit-and-priority.md#stack-measurement-with-two-workers). The bound comes from [K42]'s permit spacing and start deadline |
| [R9] | 5 | `TestPriorityOrder`, `TestPriorityTieBreak` |
| [R10] | 2, 8 | `TestValidate`, `TestBatchInvalidItem` |
| [R11] | 2, 3, 4, 8 | `TestReplayAfterSent`, `TestIdempotencyKeyHeader`, `TestReplayAfterLimitLowered`, `TestConcurrentRepeatCommit`, `TestConcurrentRepeatRollback`, `TestSingleLease`, `TestExpiredLeaseCannotOverwrite`, `TestLate202AfterFailed`, `TestStaleWriteAfterRecovery`, `TestUnclearOutcomeRetries`, `TestBatchIdempotentRepeat` |
| [R12] | 3, 4 | `TestDeliverySent`, `TestNoRedirect`, `TestClassify`, `TestSentOnFifthAttempt`, `TestFailsAfterFiveTemporary`, `TestPermanentAfterTemporary`, `TestPermanentErrorFailsOnce` |
| [R13] | 3, 4, 10 | `TestDeliverySent`, webhook.site check in [04](slices/04-retries-and-recovery.md#webhooksite-integration-check), repeated in [10](slices/10-core-release-check.md) |
| [R14] | 9 | `TestMetricsQueueDepth`, `TestMetricsProcessing`, `TestMetricsCompletions`, `TestMetricsWindowEdges`, `TestMetricsLatency`, `TestMetricsSingleSnapshot` |
| [R15] | 2, 3, 8 | `TestCorrelationIDHeader`, `TestCorrelationIDEndToEnd`, `TestBatchCorrelationID` |
| [R16] | 1 | `TestHealthEndpoints`, `TestReadyzDuringShutdown`, `docker compose ps` health |
| [N1] | 5 | `TestBurstAccepted` |
| [N2] | 3, 5 | `TestSingleLease`, `TestRateLimitTwoWorkers` |
| [N3] | 4 | `TestLeaseRecovery`, `TestRecoveryConcurrent`, `TestRecoveryLoop` |
| [B1] | 11 | Every scenario in [11](slices/11-circuit-breaker.md#failure-scenarios) |
| [B2] | 12 | `TestScheduledNotClaimedEarly` and the scenarios in [12](slices/12-scheduled-notifications.md) |
| [B3] | 14 | `TestRenderTemplate`, `TestCreateTemplate`, `TestTemplateFieldRules`, `TestTemplateRequestRules`, `TestTemplateUnknownID`, `TestListTemplates` |
| [B4] | 13 | `TestWebSocketSnapshotThenUpdate`, `TestSnapshotNoGap`, `TestBatchVersionPerNotification`, `TestBatchSnapshotMaxSize`, `TestListenerDropClosesSockets`, `TestSlowClientDisconnected`, `TestWebSocketShutdown`, `TestWebSocketWhileListenerDown`, `TestListenerDropDuringUpgrade`, `TestWebSocketNoLeak`, and the other tests in [13](slices/13-realtime-status.md#tests) |
| [B5] | 15 | `TestStoredTraceContext`, `TestAttemptSpanLink`, `TestProviderTraceparent`, `TestTransportOrder`, `TestTracingCollectorDown` |
| [B6] | 1, 16 | `gh pr checks` on every pull request; `gh run list` on `main` |
| [D1] | 10, 16 | `git log --first-parent --oneline main` |
| [D2] | every slice, 10, 16 | Every README command run against a clean clone |
| [D3] | 1, 10, 16 | `docker compose up -d --build` with a bounded health wait |
| [D4] | 1, 2, 10, 16 | `TestOpenAPIUpToDate`, and the core paths in the served document ([10](slices/10-core-release-check.md)) |
| [D5] | 1, 10, 16 | `TestMigrations`, first and second `migrate up` on a clean volume, broken-migration check in [01](slices/01-walking-skeleton.md#broken-migration) |
| [D6] | 1, 10, 16 | `go test -count=1 ./...` |
| [A1] | 10 | README known limits |
| [A2] | 3 | `TestDeliverySent` (one `PROVIDER_URL`, channel in the body) |

## Open questions

None at present. A gap found during a slice is added here and settled with a decision card before the slice depends on it.

### Settled before implementation

Planning found twelve points the records left open. Each was settled with a decision card before implementation and recorded as [K36]–[K47]; the slice notes follow those entries.

| # | Question | Settled by | Applied in slices |
|---|---|---|---|
| 1 | Tools for stack-level checks: the load generator, counting send starts, the WebSocket client | [K43] | 5, 13 |
| 2 | Host ports | [K39] | 1, 3, 4, 10 |
| 3 | Where lease recovery runs and how often | [K40] | 4 |
| 4 | Responses [K7] does not list, `3xx` included | [K41] | 4 |
| 5 | `limit` above 200 | [K44] | 7, 14 |
| 6 | Goroutine leak check | [K45] | 13 |
| 7 | Bounding actual send starts | [K42] | 5, 15 |
| 8 | Notification ID type | [K36] | 1, 2, 14 |
| 9 | How priority and status are stored | [K37] | 1 |
| 10 | PostgreSQL image tag | [K38] | 1 |
| 11 | Template details [K33] leaves open | [K47] | 14 |
| 12 | WebSocket requests while the listener is down | [K46] | 13 |

[R1]: REQUIREMENTS.md#notification-management-api
[R2]: REQUIREMENTS.md#notification-management-api
[R3]: REQUIREMENTS.md#notification-management-api
[R4]: REQUIREMENTS.md#notification-management-api
[R5]: REQUIREMENTS.md#notification-management-api
[R6]: REQUIREMENTS.md#notification-management-api
[R7]: REQUIREMENTS.md#processing-engine
[R8]: REQUIREMENTS.md#processing-engine
[R9]: REQUIREMENTS.md#processing-engine
[R10]: REQUIREMENTS.md#processing-engine
[R11]: REQUIREMENTS.md#processing-engine
[R12]: REQUIREMENTS.md#delivery--retry-left-to-the-candidates-design
[R13]: REQUIREMENTS.md#external-provider
[R14]: REQUIREMENTS.md#observability
[R15]: REQUIREMENTS.md#observability
[R16]: REQUIREMENTS.md#observability
[N1]: REQUIREMENTS.md#non-functional-requirements-from-the-case-text
[N2]: REQUIREMENTS.md#non-functional-requirements-from-the-case-text
[N3]: REQUIREMENTS.md#non-functional-requirements-from-the-case-text
[B1]: REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[B2]: REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[B3]: REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[B4]: REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[B5]: REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[B6]: REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[D1]: REQUIREMENTS.md#deliverables
[D2]: REQUIREMENTS.md#deliverables
[D3]: REQUIREMENTS.md#deliverables
[D4]: REQUIREMENTS.md#deliverables
[D5]: REQUIREMENTS.md#deliverables
[D6]: REQUIREMENTS.md#deliverables
[A1]: REQUIREMENTS.md#assumptions
[A2]: REQUIREMENTS.md#assumptions
[K0]: DECISIONS.md#k0--local-container-runtime-docker-desktop
[K4]: DECISIONS.md#k4--git-workflow-one-branch-and-pull-request-per-slice-merged-with-a-merge-commit
[K5]: DECISIONS.md#k5--rate-limiter-one-shared-permit-row-per-channel-in-postgresql
[K7]: DECISIONS.md#k7--retry-policy-retry-by-error-type-exponential-backoff-with-jitter-five-attempts-in-total
[K9]: DECISIONS.md#k9--status-model-and-cancellation-five-statuses-and-pending-means-queued
[K12]: DECISIONS.md#k12--batches-all-or-nothing-with-mixed-channels-and-priorities
[K13]: DECISIONS.md#k13--listing-filter-dates-on-created_at-paginate-with-a-created_at-id-cursor
[K19]: DECISIONS.md#k19--openapi-generated-from-the-code-with-huma-on-the-standard-library-router
[K20]: DECISIONS.md#k20--metrics-a-json-endpoint-computed-from-postgresql
[K22]: DECISIONS.md#k22--test-database-testcontainers-with-the-same-postgresql-image-as-compose
[K24]: DECISIONS.md#k24--real-time-status-events-b4-postgresql-listennotify
[K26]: DECISIONS.md#k26--scheduled-notifications-b2-an-optional-absolute-scheduled_at
[K27]: DECISIONS.md#k27--distributed-tracing-b5-opentelemetry-over-otlphttp-linked-traces-jaeger-as-an-optional-profile
[K28]: DECISIONS.md#k28--linter-golangci-lints-standard-set-plus-bodyclose-and-noctx
[K29]: DECISIONS.md#k29--ci-b6-a-github-actions-pipeline-for-lint-tests-the-openapi-check-and-the-docker-build
[K30]: DECISIONS.md#k30--health-checks-r16-healthz-for-liveness-readyz-for-readiness
[K32]: DECISIONS.md#k32--configuration-typed-environment-variables-with-caarlos0env
[K33]: DECISIONS.md#k33--templates-b3-immutable-stored-templates-with-name-placeholders-rendered-at-the-api
[K35]: DECISIONS.md#k35--planning-with-one-delivery-plan-and-a-note-per-slice
[K36]: DECISIONS.md#k36--notification-ids-uuidv7-generated-by-postgresql
[K37]: DECISIONS.md#k37--priority-and-status-two-postgresql-enum-types
[K38]: DECISIONS.md#k38--postgresql-image-postgres187-trixie-pinned
[K39]: DECISIONS.md#k39--host-ports-configurable-bound-to-127001
[K40]: DECISIONS.md#k40--lease-recovery-a-bounded-step-in-every-worker
[K41]: DECISIONS.md#k41--responses-k7-does-not-list-terminal-protocol-errors
[K42]: DECISIONS.md#k42--send-starts-a-permit-with-a-start-deadline
[K43]: DECISIONS.md#k43--measurement-tools-small-go-programs-in-the-repository
[K44]: DECISIONS.md#k44--page-size-50-by-default-1-to-200-422-outside
[K45]: DECISIONS.md#k45--goroutine-leak-check-goleak
[K46]: DECISIONS.md#k46--websocket-requests-while-the-listener-is-down-503
[K47]: DECISIONS.md#k47--template-details-a-paged-list-422-for-an-unknown-template-in-a-body
