#!/usr/bin/env bash
# Fresh-clone release check (slice 10, K4, K29).
#
#   scripts/release-check.sh [--env-out <file>] [PR]
#   scripts/release-check.sh --cleanup <file>
#
# Clones the pull request's head commit into a new temporary folder, starts it
# as its own Compose project and checks the history, the start, the
# migrations, the OpenAPI document, the tests and CI. Each step passes or
# prints "FAIL: <step>" and exits with 1; on a failure the run's own project
# (down -v on that project only) and its clone are removed. A passed run
# prints PASS, leaves its stack running for the README commands and, with
# --env-out, writes the RELEASE_* assignments into that file. --cleanup reads
# such a file and removes that run's stack and clone after checking it.
set -euo pipefail

ORIG=$(cd "$(dirname "$0")/.." && pwd)
TMP_ROOT=${TMPDIR:-/tmp}
TMP_ROOT=${TMP_ROOT%/}

fail() {
  echo "FAIL: $*"
  exit 1
}

step() {
  echo "== $*"
}

usage() {
  echo "usage: $0 [--env-out <file>] [PR] | --cleanup <file>" >&2
  exit 2
}

# --- clean-up ----------------------------------------------------------------

cleanup() {
  local file=$1
  # Only the given file decides what is removed; nothing from the shell.
  unset COMPOSE_PROJECT_NAME RELEASE_PROJECT RELEASE_SHA RELEASE_WORK RELEASE_DIR RELEASE_ORIG

  [ -s "$file" ] || fail "cleanup the file is empty or missing"
  local names
  names=$(grep -v '^[[:space:]]*$' "$file" | sed -n 's/^\([A-Z_]*\)=.*/\1/p' | sort | tr '\n' ' ')
  [ "$names" = "RELEASE_DIR RELEASE_ORIG RELEASE_PROJECT RELEASE_SHA RELEASE_WORK " ] ||
    fail "cleanup the file must assign exactly RELEASE_PROJECT, RELEASE_SHA, RELEASE_WORK, RELEASE_DIR and RELEASE_ORIG"
  [ "$(grep -cv '^[[:space:]]*$' "$file")" = 5 ] || fail "cleanup the file holds more than the five assignments"
  # shellcheck disable=SC1090
  . "$file"

  [[ $RELEASE_PROJECT =~ ^nsys-release-[0-9a-f]+-[0-9]+$ ]] || fail "cleanup project name $RELEASE_PROJECT"
  local main
  main=$(cd "$RELEASE_ORIG" && docker compose config --format json | jq -r .name) || fail "cleanup main project name"
  [ "$RELEASE_PROJECT" != "$main" ] || fail "cleanup the project is the main stack's"

  [ -d "$RELEASE_WORK" ] || fail "cleanup work folder missing"
  case $RELEASE_WORK in "$TMP_ROOT"/*) ;; *) fail "cleanup work folder is not under $TMP_ROOT" ;; esac
  [ -f "$RELEASE_WORK/.nsys-release" ] || fail "cleanup marker missing"
  [ "$(cat "$RELEASE_WORK/.nsys-release")" = "$RELEASE_PROJECT $RELEASE_SHA" ] || fail "cleanup marker does not match"
  [ "$(dirname "$RELEASE_DIR")" = "$RELEASE_WORK" ] || fail "cleanup clone is not directly inside the work folder"
  [ "$(git -C "$RELEASE_DIR" rev-parse HEAD)" = "$RELEASE_SHA" ] || fail "cleanup clone is not at $RELEASE_SHA"

  (cd "$RELEASE_DIR" && docker compose -p "$RELEASE_PROJECT" down -v >/dev/null 2>&1) || fail "cleanup down"
  rm -rf "$RELEASE_WORK"
  : >"$file"
  echo "CLEANED $RELEASE_PROJECT"
}

if [ "${1:-}" = "--cleanup" ]; then
  [ $# -eq 2 ] || usage
  cleanup "$2"
  exit 0
fi

ENV_OUT=""
if [ "${1:-}" = "--env-out" ]; then
  [ $# -ge 2 ] || usage
  ENV_OUT=$2
  shift 2
fi
[ $# -le 1 ] || usage
PR=${1:-}

# --- 1. candidate --------------------------------------------------------------

step candidate
cd "$ORIG"
if [ -z "$PR" ]; then
  PR=$(gh pr view --json number -q .number 2>/dev/null) || fail "candidate no pull request for the current branch"
fi
SHA=$(gh pr view "$PR" --json headRefOid -q .headRefOid 2>/dev/null) || fail "candidate pull request $PR"
[[ $SHA =~ ^[0-9a-f]{40}$ ]] || fail "candidate head SHA $SHA"
echo "pr=$PR sha=$SHA"

# --- 2. host ports -------------------------------------------------------------

step ports
for spec in POSTGRES_PORT:5433 API_PORT:8080 MOCK_PORT:8081; do
  name=${spec%%:*}
  port=${!name:-${spec#*:}}
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    fail "ports $name $port is in use; stop the main stack first"
  fi
done
if [ -n "$ENV_OUT" ]; then
  [ -f "$ENV_OUT" ] && [ ! -s "$ENV_OUT" ] && [ -w "$ENV_OUT" ] ||
    fail "ports --env-out $ENV_OUT must exist, be empty and be writable"
fi
API="http://127.0.0.1:${API_PORT:-8080}"

# --- 3. clone ------------------------------------------------------------------

step clone
PROJECT="nsys-release-${SHA:0:7}-$(date +%s)"
WORK=$(mktemp -d "$TMP_ROOT/nsys-release.XXXXXX")
DIR="$WORK/repo"
PASSED=0
on_exit() {
  if [ "$PASSED" != 1 ]; then
    if [ -d "$DIR" ]; then
      (cd "$DIR" && docker compose -p "$PROJECT" down -v >/dev/null 2>&1) || true
    fi
    rm -rf "$WORK"
  fi
}
trap on_exit EXIT

git clone -q "$(git -C "$ORIG" remote get-url origin)" "$DIR" || fail clone
git -C "$DIR" fetch -q origin "pull/$PR/head" || fail "clone fetch pull/$PR/head"
git -C "$DIR" checkout -q --detach "$SHA" || fail "clone checkout $SHA"
[ "$(git -C "$DIR" rev-parse HEAD)" = "$SHA" ] || fail "clone HEAD is not $SHA"
git -C "$DIR" merge-base --is-ancestor origin/main "$SHA" || fail "clone $SHA does not contain origin/main"
echo "project=$PROJECT clone=$DIR"

# --- 4. history (D1) -----------------------------------------------------------

step history
non_merges=$(git -C "$DIR" rev-list --first-parent --max-parents=1 origin/main)
root=$(git -C "$DIR" rev-list --max-parents=0 origin/main)
[ "$non_merges" = "$root" ] || fail "history main's first-parent line has commits other than the root that are not merges"
echo "merges=$(git -C "$DIR" rev-list --first-parent --min-parents=2 origin/main | wc -l | tr -d ' ') root=${root:0:7}"

# --- 5. start (D3) -------------------------------------------------------------

step start
cd "$DIR"
PROVIDER_URL=http://mockprovider:8081/ PROVIDER_PROFILE=standard MOCK_DELAY=0s \
  docker compose -p "$PROJECT" up -d --build >/dev/null 2>&1 || fail "start compose up"
scripts/wait-health.sh -p "$PROJECT" api healthy 60 || fail "start api not healthy"
states=$(docker compose -p "$PROJECT" ps -a --format '{{.Service}} {{.State}} {{.Health}} {{.ExitCode}}' | sort)
echo "$states"
grep -qx 'postgres running healthy 0' <<<"$states" || fail "start postgres"
grep -qx 'migrate exited  0' <<<"$states" || fail "start migrate"
grep -q '^worker running ' <<<"$states" || fail "start worker"
grep -q '^mockprovider running ' <<<"$states" || fail "start mockprovider"

# --- 6. migrations (D5) --------------------------------------------------------

step migrations
out=$(docker compose -p "$PROJECT" run --rm migrate 2>/dev/null) || fail "migrations second run"
grep -q '"no migrations to apply"' <<<"$out" || fail "migrations second run applied something"
! grep -q '"migration applied"' <<<"$out" || fail "migrations second run applied something"
applied=$(docker compose -p "$PROJECT" exec -T postgres psql -U notifications -d notifications -Atc \
  "SELECT DISTINCT version_id FROM goose_db_version WHERE is_applied AND version_id > 0 ORDER BY 1")
files=$(for f in migrations/*.sql; do b=${f##*/}; echo $((10#${b%%_*})); done | sort -n)
[ "$applied" = "$files" ] || fail "migrations applied versions $(echo $applied) differ from files $(echo $files)"
echo "second run: no migrations to apply; applied versions: $(echo $applied)"

# --- 7. OpenAPI (D4) -----------------------------------------------------------

step openapi
doc="$WORK/openapi.json"
curl -sf "$API/openapi.json" -o "$doc" || fail "openapi GET /openapi.json"
for p in /healthz /readyz /notifications '/notifications/{id}' '/notifications/{id}/cancel' \
  /notifications/batch '/batches/{id}' /metrics; do
  jq -e --arg p "$p" '.paths[$p]' "$doc" >/dev/null || fail "openapi path $p missing"
done
need() { # path method codes...
  local path=$1 method=$2
  shift 2
  for code in "$@"; do
    jq -e --arg p "$path" --arg m "$method" --arg c "$code" '.paths[$p][$m].responses[$c]' "$doc" >/dev/null ||
      fail "openapi $method $path has no $code response"
  done
}
need /notifications post 202 400 422
need /notifications/batch post 202 400 413 422
need '/notifications/{id}/cancel' post 200 404 409
need /readyz get 200 503
go test -count=1 -run '^TestOpenAPIUpToDate$' ./internal/httpapi/ >/dev/null || fail "openapi TestOpenAPIUpToDate"
echo "8 paths and the required responses present; TestOpenAPIUpToDate ok"

# --- 8. tests (D6) -------------------------------------------------------------

step tests
tests="$WORK/tests.txt"
go test -count=1 ./... >"$tests" 2>&1 || {
  grep -E '^(FAIL|---)' "$tests" | head -20
  fail tests
}
! grep -q '(cached)' "$tests" || fail "tests a result was cached"
grep -E '^ok' "$tests" | awk '{print $1, $2}'

# --- 9. CI (B6) ----------------------------------------------------------------

step ci
cd "$ORIG"
run=$(gh run list --commit "$SHA" --workflow ci.yml --event pull_request --limit 1 \
  --json databaseId,status,conclusion -q '.[0] | "\(.databaseId) \(.status) \(.conclusion)"') || fail "ci run list"
read -r run_id run_status run_conclusion <<<"$run"
[ "$run_status" = completed ] && [ "$run_conclusion" = success ] || fail "ci run $run_id is $run_status/$run_conclusion"
# The log goes to a file first: grep -q would stop reading early, and with
# pipefail the writer's broken pipe would fail the step.
gh run view "$run_id" --log >"$WORK/ci.log" 2>/dev/null || fail "ci run $run_id log"
grep -q "tested-sha=$SHA" "$WORK/ci.log" || fail "ci run $run_id has no tested-sha=$SHA"
echo "run=$run_id conclusion=success tested-sha=$SHA"

# --- 10. result ----------------------------------------------------------------

printf '%s %s' "$PROJECT" "$SHA" >"$WORK/.nsys-release"
if [ -n "$ENV_OUT" ]; then
  {
    printf 'RELEASE_PROJECT=%q\n' "$PROJECT"
    printf 'RELEASE_SHA=%q\n' "$SHA"
    printf 'RELEASE_WORK=%q\n' "$WORK"
    printf 'RELEASE_DIR=%q\n' "$DIR"
    printf 'RELEASE_ORIG=%q\n' "$ORIG"
  } >"$ENV_OUT"
fi
PASSED=1
echo "PASS sha=$SHA project=$PROJECT clone=$DIR"
