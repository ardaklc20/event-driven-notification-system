#!/usr/bin/env bash
# webhook.site integration check (R13, K14).
#
#   scripts/webhook-check.sh
#
# Creates a free webhook.site URL that answers 202 with a body in the case's
# shape, starts its own Compose project (empty database, one worker) pointed at
# that URL with the webhook_site_free profile, sends one synthetic
# notification, and checks both sides: our row is sent with the run's
# messageId and timestamp, and webhook.site received exactly one POST with the
# expected body and correlation ID.
#
# Every step either passes or prints "FAIL: <step>" and exits with 1; "PASS"
# comes only when all of them passed. The project is removed on every exit.
# The full URL is never printed: anyone who knows a free URL's ID can read
# what it received, so only the token's first group is shown.
#
# Stop the main stack first (docker compose stop): the host ports must be free.
# webhook.site allows one new free URL per minute.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() {
  echo "FAIL: $*"
  exit 1
}

for tool in docker curl jq; do
  command -v "$tool" >/dev/null || fail "tools: $tool is not installed"
done

# 1. Host ports, read like Compose reads them (K39).
BIND=${BIND_ADDRESS:-127.0.0.1}
API="http://$BIND:${API_PORT:-8080}"
for port in "${POSTGRES_PORT:-5433}" "${API_PORT:-8080}" "${MOCK_PORT:-8081}"; do
  if (exec 3<>"/dev/tcp/$BIND/$port") 2>/dev/null; then
    fail "host ports: $BIND:$port is in use; stop the main stack first (docker compose stop)"
  fi
done

# 2. Names for this run.
EPOCH=$(date +%s)
PROJECT="nsys-webhook-$EPOCH"
CORRELATION_ID="wh-check-$EPOCH"
MESSAGE_ID="msg-$CORRELATION_ID"
TIMESTAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)
PAYLOAD='{"to":"+905551234567","channel":"sms","content":"webhook.site check"}'
LOG=$(mktemp -d)

cleanup() {
  docker compose -p "$PROJECT" down -v >/dev/null 2>&1 || true
  rm -rf "$LOG"
}
trap cleanup EXIT

# 3. A free URL whose default response is a 202 in the case's shape.
CONTENT=$(jq -nc --arg id "$MESSAGE_ID" --arg ts "$TIMESTAMP" '{messageId: $id, status: "accepted", timestamp: $ts}')
REQUEST=$(jq -nc --arg c "$CONTENT" '{default_status: 202, default_content_type: "application/json", default_content: $c}')
curl -sS --max-time 15 -X POST https://webhook.site/token \
  -H 'Content-Type: application/json' -H 'Accept: application/json' \
  -d "$REQUEST" >"$LOG/token.json" || fail "URL: the token request failed"
TOKEN=$(jq -r '.uuid // empty' "$LOG/token.json" 2>/dev/null || true)
[ -n "$TOKEN" ] || fail "URL: no token in webhook.site's answer (one new free URL per minute)"
MASKED="https://webhook.site/${TOKEN%%-*}-…"

# 4. The stack, pointed at that URL with the free profile.
PROVIDER_URL="https://webhook.site/$TOKEN" PROVIDER_PROFILE=webhook_site_free \
  docker compose -p "$PROJECT" up -d --build >"$LOG/up.log" 2>&1 ||
  { tail -20 "$LOG/up.log"; fail "stack: docker compose up"; }
scripts/wait-health.sh -p "$PROJECT" api healthy 60 >/dev/null || fail "stack: the API did not become healthy"

# 5. Preconditions: one worker, empty database.
WORKERS=$(docker compose -p "$PROJECT" ps -q worker | wc -l | tr -d ' ')
[ "$WORKERS" = 1 ] || fail "preconditions: $WORKERS worker containers, want 1"
ROWS=$(docker compose -p "$PROJECT" exec -T postgres \
  psql -U notifications -d notifications -tAc 'select count(*) from notifications') ||
  fail "preconditions: could not count notifications"
[ "$ROWS" = 0 ] || fail "preconditions: the database holds $ROWS notifications, want 0"

# 6. One notification, followed until it ends.
CREATED=$(curl -sS --max-time 10 -X POST "$API/notifications" \
  -H 'Content-Type: application/json' -H "X-Correlation-ID: $CORRELATION_ID" \
  -d '{"recipient":"+905551234567","channel":"sms","content":"webhook.site check"}') ||
  fail "notification: create request failed"
ID=$(jq -r '.id // empty' <<<"$CREATED")
[ -n "$ID" ] || fail "notification: no id in $CREATED"
STATUS=unknown
for _ in $(seq 1 30); do
  DETAIL=$(curl -sS --max-time 5 "$API/notifications/$ID") || fail "notification: read failed"
  STATUS=$(jq -r .status <<<"$DETAIL")
  case $STATUS in sent | failed) break ;; esac
  sleep 1
done

# 7. Our side.
[ "$STATUS" = sent ] || fail "our side: status $STATUS, want sent (last_error: $(jq -r .last_error <<<"$DETAIL"))"
GOT_ID=$(jq -r .provider_message_id <<<"$DETAIL")
[ "$GOT_ID" = "$MESSAGE_ID" ] || fail "our side: provider_message_id $GOT_ID, want $MESSAGE_ID"
GOT_TS=$(jq -r .provider_timestamp <<<"$DETAIL")
[ "$GOT_TS" = "$TIMESTAMP" ] || fail "our side: provider_timestamp $GOT_TS, want $TIMESTAMP"

# 8. webhook.site's side: exactly one request with this run's correlation ID.
curl -sS --max-time 15 -H 'Accept: application/json' \
  "https://webhook.site/token/$TOKEN/requests?sorting=newest" >"$LOG/requests.json" ||
  fail "webhook.site: could not list the received requests"
jq --arg c "$CORRELATION_ID" '
  [.data[]
   | select((.headers | to_entries | map(select(.key | ascii_downcase == "x-correlation-id")) | .[0].value
             | if type == "array" then .[0] else . end) == $c)]' "$LOG/requests.json" >"$LOG/match.json" 2>/dev/null ||
  fail "webhook.site: unexpected answer from the request list"
MATCHES=$(jq length "$LOG/match.json")
[ "$MATCHES" = 1 ] || fail "webhook.site: $MATCHES requests carry the correlation ID, want 1"
METHOD=$(jq -r '.[0].method' "$LOG/match.json")
[ "$METHOD" = POST ] || fail "webhook.site: method $METHOD, want POST"
GOT_BODY=$(jq -r '.[0].content' "$LOG/match.json" | jq -S . 2>/dev/null) || fail "webhook.site: the body is not JSON"
WANT_BODY=$(jq -S . <<<"$PAYLOAD")
[ "$GOT_BODY" = "$WANT_BODY" ] || fail "webhook.site: body $(jq -c . <<<"$GOT_BODY"), want $PAYLOAD"

# 9. Evidence.
echo "date:           $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "url:            $MASKED"
echo "correlation_id: $CORRELATION_ID"
echo "payload:        $(jq -c . <<<"$GOT_BODY")"
echo "status:         $STATUS"
echo "message_id:     $GOT_ID"
echo "PASS"
