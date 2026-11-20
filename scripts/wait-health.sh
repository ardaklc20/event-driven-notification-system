#!/usr/bin/env bash
# Bounded wait for a Compose service's container health.
#
#   scripts/wait-health.sh [-p project] <service> <state> <tries>
#
# Reads the health state every 2 seconds. Prints "<service>=<state>" and exits
# with 0 once the wanted state is seen; after the last try it prints the last
# state seen and exits with 1, so a failed wait fails the check that ran it.
set -euo pipefail

usage() {
  echo "usage: $0 [-p project] <service> <healthy|unhealthy|starting> <tries>" >&2
  exit 2
}

if [ "${1:-}" = "-p" ]; then
  [ $# -ge 2 ] || usage
  export COMPOSE_PROJECT_NAME="$2"
  shift 2
fi
[ $# -eq 3 ] || usage
service=$1
want=$2
tries=$3
case $tries in '' | *[!0-9]*) usage ;; esac

cd "$(dirname "$0")/.."

last=none
for ((i = 1; i <= tries; i++)); do
  id=$(docker compose ps -a -q "$service" 2>/dev/null | head -n 1 || true)
  if [ -z "$id" ]; then
    last=no-container
  else
    last=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}-no-healthcheck{{end}}' "$id" 2>/dev/null || echo unknown)
    if [ "$last" = "$want" ]; then
      echo "$service=$last"
      exit 0
    fi
  fi
  if [ "$i" -lt "$tries" ]; then
    sleep 2
  fi
done

echo "$service=$last (wanted $want after $tries tries)"
exit 1
