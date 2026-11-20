# Slice 10 — Core release check

[Delivery plan](../PLAN.md) · Branch `docs/core-readme` · Core · Depends on: slices 1–9

## Goal

The core is a deliverable on its own. On a fresh clone **of this pull request's head commit**, one command starts it, one command tests it, its OpenAPI document matches the core API, its migrations apply once and then do nothing, the README's commands work as written, and the webhook.site integration has real, recorded evidence.

## What this slice delivers

| ID | Delivered here |
|---|---|
| [D1] | A history of merge commits, one per pull request ([K4]) |
| [D2] | A complete core README |
| [D3] | One-command start, checked on a fresh clone |
| [D4] | The served document lists every core path and its responses |
| [D5] | First and second migration runs on a clean volume |
| [D6] | One-command tests, on the fresh clone |
| [R13] | The webhook.site check repeated for this release ([K14]) |
| [A1] | No authentication, stated as a limit |

## Preconditions

- Slices 1–9 merged, with their status `done` in the plan.
- This slice's README and `AI_USAGE.md` changes are committed and pushed, and its pull request is open. The release check runs on the pull request's head commit; after any new commit it runs again.

## Files

| Path | Purpose |
|---|---|
| `scripts/release-check.sh` | The fresh-clone check below; stops at the first failure and exits with 1 |
| `README.md`, `AI_USAGE.md` | Completed as below |

## README contents to complete

| Section | Content |
|---|---|
| Overview | What the system does; the four programs; the table as the queue ([K2]) |
| Setup | Docker; `docker compose up`; ports; how to stop and reset |
| Architecture | API → `notifications` → worker → provider; the claim, lease and permit in one paragraph each ([K3], [K5], [K9]) |
| API examples | One working command per core endpoint: create, get, list, cancel, batch create, batch status, metrics, health |
| Configuration | Every environment variable with its default ([K32]) |
| Testing | `go test -count=1 ./...`; Docker must be running ([K22]) |
| webhook.site | How to run `scripts/webhook-check.sh`, and the run recorded in slice 4 (date, masked URL, correlation ID, final status, `messageId`). The README keeps that verified run; this slice's run goes into the pull request only, so recording it does not change the commit under test ([K14]) |
| Known limits | No authentication ([A1]); possible duplicate sends ([K3]); global keys kept as long as their record ([K10]); SMS limit counted in code points, not segments ([K11]); `low` can starve ([K8]); pages are not one snapshot ([K13]); `messageId` is not unique ([K15]); the rate-limit measurement and what it does not prove ([K5]); no delivery step in CI ([K29]) |
| Bonuses | Only CI ([B6]) at this point; the others are listed as planned, not as done |

## Steps

1. Complete `README.md` as above.
2. Fill `AI_USAGE.md` section 4 for R1–R16 and N1–N3 with the checks that were actually run in slices 1–9 and their observed results.
3. Write `scripts/release-check.sh`; commit and push everything.
4. Run the release check, then the README commands and the webhook.site check, as below.
5. If a README command does not work as written, fix it, commit, push, and run step 4 again on the new head commit.

## Release check

`scripts/release-check.sh [--env-out <file>] [PR]` runs from the working checkout; `scripts/release-check.sh --cleanup <file>` removes a passed run ([below](#clean-up)). It uses `set -euo pipefail`; each step either passes or prints `FAIL: <step>` and exits with 1. On a failure, a trap removes the script's own Compose project (`down -v` on that project only) and its clone. Every Compose command uses one release project whose name is unique to this run, so its `down -v` can only remove this run's volume, whatever the clone folder is called.

1. **Candidate.** The pull request (argument, or the one for the current branch) and its head SHA from `gh pr view`.
2. **Host ports.** The script reads the same variables as Compose ([K39]): `POSTGRES_PORT`, `API_PORT` and `MOCK_PORT` (5433, 8080 and 8081 by default) must be free; otherwise it fails with "stop the main stack first". If `--env-out` is given, the file must exist, be empty and be writable; otherwise it fails here, before anything starts.
3. **Clone.** Project `nsys-release-<short SHA>-<epoch>`; a new temporary folder; `git clone` of `origin`; `git fetch origin pull/<PR>/head`; `git checkout --detach <SHA>`; `git rev-parse HEAD` must equal the SHA.
4. **History ([D1]).** `git rev-list --first-parent --max-parents=1 origin/main` returns exactly one commit, the root; every other commit on `main`'s first-parent line is a merge.
5. **Start ([D3]).** `docker compose -p <project> up -d --build` with `PROVIDER_URL=http://mockprovider:8081/` and `PROVIDER_PROFILE=standard` set on that command, so the shell's environment cannot point the release stack at a real provider. Then `scripts/wait-health.sh -p <project> api healthy 60`, and `postgres` healthy, `migrate` exited 0, `worker` and `mockprovider` running.
6. **Migrations ([D5]).** `docker compose -p <project> run --rm migrate` exits with 0 and logs no applied migration. The applied versions in `goose_db_version` equal, as a set, the versions of the SQL files in the checkout.
7. **OpenAPI ([D4]).** `GET /openapi.json` lists `/healthz`, `/readyz`, `/notifications`, `/notifications/{id}`, `/notifications/{id}/cancel`, `/notifications/batch`, `/batches/{id}` and `/metrics`. Required response codes, checked with `jq -e`: `POST /notifications` `202`, `400`, `422`; `POST /notifications/batch` `202`, `400`, `413`, `422`; `POST /notifications/{id}/cancel` `200`, `404`, `409`; `GET /readyz` `200`, `503`. `TestOpenAPIUpToDate` passes.
8. **Tests ([D6]).** `go test -count=1 ./...` in the clone exits with 0, and no line contains `(cached)`.
9. **CI ([B6]).** The latest `ci.yml` run for the SHA (`gh run list --commit <SHA> --workflow ci.yml`) has the conclusion `success`. A missing or unfinished run fails.
10. **Result.** Prints `PASS`, the SHA, the project name and the clone folder, and leaves that stack running for the README commands. Only now, after every step has passed, it writes a marker file `.nsys-release` into the clone's temporary folder holding the project name and the SHA, and writes `RELEASE_PROJECT`, `RELEASE_SHA`, `RELEASE_WORK` (the temporary folder), `RELEASE_DIR` (the clone inside it) and `RELEASE_ORIG` (the working checkout) as shell assignments, each value quoted with `printf %q`, into the `--env-out` file. A failed run never writes either file, so the `--env-out` file stays empty. The script has no fixed metadata path: each run names its own file. It never writes `COMPOSE_PROJECT_NAME`, so a value already in the shell can never pass for a release project.

**Then, in one shell:**

```bash
docker compose stop                                   # the main stack; keeps its volume
unset RELEASE_PROJECT RELEASE_SHA RELEASE_WORK RELEASE_DIR RELEASE_ORIG
RUN_ENV=$(mktemp "${TMPDIR:-/tmp}/nsys-release.XXXXXX")  # this run's own file, empty
scripts/release-check.sh --env-out "$RUN_ENV" && . "$RUN_ENV" && echo "release env loaded: $RELEASE_PROJECT"
```

The `RELEASE_*` names are cleared first, so after this block they are set only if this run passed and its file was loaded. Go on only if the last line is `release env loaded: nsys-release-…`. If the script failed, nothing was loaded: run `rm -f "$RUN_ENV"; docker compose start` and stop here.

```bash
cd "${RELEASE_DIR:?release env not loaded}" && \
  export COMPOSE_PROJECT_NAME="${RELEASE_PROJECT:?release env not loaded}" && \
  unset PROVIDER_URL PROVIDER_PROFILE && \
  echo "README commands now target $COMPOSE_PROJECT_NAME"
# run every command block of README.md, in order, against the release project
docker compose stop                                   # the release project; frees the ports
scripts/webhook-check.sh; echo "exit=$?"
```

`COMPOSE_PROJECT_NAME` is set only from the loaded `RELEASE_PROJECT`, and only once the `cd` into the clone has worked; without the line `README commands now target nsys-release-…`, do not go on.

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Release check ([D1], [D3], [D4], [D5], [D6], [B6]) | `scripts/release-check.sh --env-out "$RUN_ENV"`, as in the run block | `PASS` with the pull request's head SHA, then `release env loaded`. Any `FAIL: <step>` with exit status 1 fails this slice, and the run's file stays empty |
| README ([D2]) | Every README command block, in order, against the release project | Each returns the status code and fields the README shows |
| webhook.site ([R13]) | `scripts/webhook-check.sh` | `PASS` and `exit=0` |
| Clean-up guard | The guard test in [Clean-up](#clean-up), with `COMPOSE_PROJECT_NAME=insidercase` and an empty file | `FAIL: cleanup` and exit status 1; the main stack's volume is still listed |
| Clean-up | `scripts/release-check.sh --cleanup "$RUN_ENV"` | `CLEANED nsys-release-…` and `exit=0`; only the release project's volume is gone |

## Evidence for the pull request

- The release script's full output, with the SHA.
- The README commands with their actual output.
- The webhook script's output (masked URL only). It stays in the pull request; the README keeps slice 4's run.

## Documentation

- `README.md` as above.
- `AI_USAGE.md`: section 4 rows for R1–R16 and N1–N3, each with the command that was run and what it showed; the slice log row for this check.

## Clean-up

Removing the release stack deletes a volume, so it is done by the script, never by a pasted `down -v`. `scripts/release-check.sh --cleanup <file>` first removes `COMPOSE_PROJECT_NAME` and every `RELEASE_*` name from its own environment, then reads only the given `--env-out` file of a passed run. Before it removes anything, it checks:

1. The file is not empty and assigns exactly `RELEASE_PROJECT`, `RELEASE_SHA`, `RELEASE_WORK`, `RELEASE_DIR` and `RELEASE_ORIG`.
2. `RELEASE_PROJECT` matches `^nsys-release-[0-9a-f]+-[0-9]+$`, and differs from the main stack's project name (`docker compose config --format json | jq -r .name` in `RELEASE_ORIG`).
3. `RELEASE_WORK` is a directory under `${TMPDIR:-/tmp}`, its `.nsys-release` marker holds exactly `RELEASE_PROJECT` and `RELEASE_SHA`, `RELEASE_DIR` lies directly inside it, and `git -C "$RELEASE_DIR" rev-parse HEAD` equals `RELEASE_SHA`.

Only then does it run `docker compose -p "$RELEASE_PROJECT" down -v` in `RELEASE_DIR`, delete `RELEASE_WORK` and empty the file, and print `CLEANED <project>`. Any failed check prints `FAIL: cleanup <check>`, exits with 1 and removes nothing.

```bash
cd "${RELEASE_ORIG:?release env not loaded}"
scripts/release-check.sh --cleanup "$RUN_ENV"; echo "exit=$?"
unset COMPOSE_PROJECT_NAME RELEASE_PROJECT RELEASE_SHA RELEASE_WORK RELEASE_DIR RELEASE_ORIG
rm -f "$RUN_ENV"
docker compose start            # the main stack
```

**Expected observation:** `CLEANED nsys-release-…` and `exit=0`; `docker volume ls` no longer lists the release project's volume, and the main stack's volume is still there.

**Guard test.** Before the first real run, the script's guard is checked against the case that broke the pasted clean-up: with `COMPOSE_PROJECT_NAME=insidercase` in the shell, `scripts/release-check.sh --cleanup "$(mktemp)"` (an empty file) prints `FAIL: cleanup` and exits with 1, and `docker volume ls` still lists the main stack's volume. The output goes into the pull request.

[R13]: ../REQUIREMENTS.md#external-provider
[B6]: ../REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[D1]: ../REQUIREMENTS.md#deliverables
[D2]: ../REQUIREMENTS.md#deliverables
[D3]: ../REQUIREMENTS.md#deliverables
[D4]: ../REQUIREMENTS.md#deliverables
[D5]: ../REQUIREMENTS.md#deliverables
[D6]: ../REQUIREMENTS.md#deliverables
[A1]: ../REQUIREMENTS.md#assumptions
[K2]: ../DECISIONS.md#k2--queue-the-notifications-table-in-postgresql-is-the-queue
[K3]: ../DECISIONS.md#k3--delivery-guarantee-a-durable-queue-lease-recovery-and-bounded-retries
[K4]: ../DECISIONS.md#k4--git-workflow-one-branch-and-pull-request-per-slice-merged-with-a-merge-commit
[K5]: ../DECISIONS.md#k5--rate-limiter-one-shared-permit-row-per-channel-in-postgresql
[K8]: ../DECISIONS.md#k8--priority-strict-order-within-each-channel
[K9]: ../DECISIONS.md#k9--status-model-and-cancellation-five-statuses-and-pending-means-queued
[K10]: ../DECISIONS.md#k10--idempotency-key-an-optional-idempotency-key-header-one-key-per-create-request
[K11]: ../DECISIONS.md#k11--validation-one-set-of-rules-at-the-api-limits-counted-in-unicode-code-points
[K13]: ../DECISIONS.md#k13--listing-filter-dates-on-created_at-paginate-with-a-created_at-id-cursor
[K14]: ../DECISIONS.md#k14--provider-a-small-go-mock-by-default-webhooksite-as-a-separate-profile
[K15]: ../DECISIONS.md#k15--provider-response-store-messageid-and-timestamp
[K22]: ../DECISIONS.md#k22--test-database-testcontainers-with-the-same-postgresql-image-as-compose
[K29]: ../DECISIONS.md#k29--ci-b6-a-github-actions-pipeline-for-lint-tests-the-openapi-check-and-the-docker-build
[K32]: ../DECISIONS.md#k32--configuration-typed-environment-variables-with-caarlos0env
[K39]: ../DECISIONS.md#k39--host-ports-configurable-bound-to-127001
