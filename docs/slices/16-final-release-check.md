# Slice 16 — Final release check

[Delivery plan](../PLAN.md) · Branch `docs/final-readme` · Core + optional · Depends on: slice 10 and every bonus slice whose status is `done`

## Goal

The finished project passes the clean-clone checks of slice 10 again, now with the bonuses that were actually completed. The README's bonus section and `AI_USAGE.md` describe exactly what was built: a bonus whose slice is not `done` is not checked here and is not presented as built.

## What this slice delivers

| ID | Delivered here |
|---|---|
| [D1]–[D6] | Rechecked on a fresh clone, including what the completed bonuses added |
| [B1]–[B6] | Only the bonuses whose slices are `done`: their checks rerun on the fresh clone |

## Preconditions

- Slice 10 merged.
- Every bonus slice is either `done` with its pull request merged, or stopped. A stopped slice has no open branch that this check would depend on.

## Which bonuses are checked

Fill this table from the status column of the delivery plan when the slice starts; it goes into the pull request.

| Bonus | Slice | Status in the plan | Checked here |
|---|---|---|---|
| [B6] | 1 | `done` since slice 1 | Yes |
| [B1] | 11 | `done` or not | Only if `done` |
| [B2] | 12 | `done` or not | Only if `done` |
| [B4] | 13 | `done` or not | Only if `done` |
| [B3] | 14 | `done` or not | Only if `done` |
| [B5] | 15 | `done` or not | Only if `done` |

## README and AI_USAGE to complete

| File | Section | Content |
|---|---|---|
| `README.md` | Bonuses | One line per bonus. Built: what it does and how to try it. Not built: "not built" and why, in one sentence. The list matches the table above exactly |
| `README.md` | Architecture, configuration, API examples | Extended for the completed bonuses only: new endpoints, settings and Compose profiles |
| `README.md` | Known limits | Those of slice 10, plus the completed bonuses' costs ([K24], [K25], [K26], [K27], [K33], [K34]) |
| `AI_USAGE.md` | Section 4 | A row for each completed bonus, with the command that was run and what it showed |
| `AI_USAGE.md` | Slice log | A row for this check |

## Steps

1. Fill the table of bonuses from the plan.
2. Complete `README.md` and `AI_USAGE.md` as above.
3. Commit, push, and run the [release check of slice 10](10-core-release-check.md#release-check) on this pull request's head commit, with its run block: `scripts/release-check.sh --env-out "$RUN_ENV"`, and the release values loaded only after it passes. It uses its own release project.
4. Against the same release project and clone, run the acceptance checks of every completed bonus slice, and its stack check. A bonus check that needs its own Compose project names one, as its slice note says. Check the completed bonuses' OpenAPI paths and response codes with `jq -e` the same way the script checks the core ones.
5. Fix any README command that does not work as written, commit, push, and run steps 3–4 again on the new head commit.
6. Clean up as in slice 10.

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Release check ([D1], [D3], [D4], [D5], [D6], [B6]) | `scripts/release-check.sh --env-out "$RUN_ENV"`, as in slice 10's run block | `PASS` with this pull request's head SHA, then `release env loaded`. Its migration step compares sets of versions, so a skipped bonus that leaves a gap in the numbering does not break it |
| Bonus OpenAPI ([D4]) | `jq -e` on `GET /openapi.json` of the release stack | `/templates` and `/templates/{id}` with their response codes only if [B3] is `done`; no path of a bonus that is not `done` |
| CI on this pull request ([B6]) | `gh pr checks` | Every job shows `pass` |
| Completed bonuses | The acceptance checks and the stack check of each bonus slice marked `done` | The expected observation listed in that slice note, for each |
| README ([D2]) | Every README command block, in order | Each returns the status code and fields the README shows |
| Bonus scope | Compare the README's bonus section with the table of bonuses | A bonus is described as built if and only if its slice is `done` |

## Evidence for the pull request

- The filled table of bonuses.
- The release script's full output with the SHA, and the output of every completed bonus's checks.
- The README commands with their actual output.

## Documentation

- `README.md` and `AI_USAGE.md` as above.

## Clean-up

As in [slice 10](10-core-release-check.md#clean-up).

[B1]: ../REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[B2]: ../REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[B3]: ../REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[B4]: ../REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[B5]: ../REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[B6]: ../REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[D1]: ../REQUIREMENTS.md#deliverables
[D2]: ../REQUIREMENTS.md#deliverables
[D3]: ../REQUIREMENTS.md#deliverables
[D4]: ../REQUIREMENTS.md#deliverables
[D5]: ../REQUIREMENTS.md#deliverables
[D6]: ../REQUIREMENTS.md#deliverables
[K24]: ../DECISIONS.md#k24--real-time-status-events-b4-postgresql-listennotify
[K25]: ../DECISIONS.md#k25--websocket-endpoint-b4-coderwebsocket-one-notification-or-batch-per-connection
[K26]: ../DECISIONS.md#k26--scheduled-notifications-b2-an-optional-absolute-scheduled_at
[K27]: ../DECISIONS.md#k27--distributed-tracing-b5-opentelemetry-over-otlphttp-linked-traces-jaeger-as-an-optional-profile
[K33]: ../DECISIONS.md#k33--templates-b3-immutable-stored-templates-with-name-placeholders-rendered-at-the-api
[K34]: ../DECISIONS.md#k34--failure-handling-b1-one-circuit-breaker-for-the-provider-shared-by-all-workers
