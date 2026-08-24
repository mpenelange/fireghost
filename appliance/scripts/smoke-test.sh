#!/bin/sh
set -eu

base_url=${1:-http://127.0.0.1:33000}
case "$base_url" in
  http://*|https://*) ;;
  *) printf '%s\n' "usage: $0 [http[s]://router-host:port]" >&2; exit 2 ;;
esac

# Load the local deployment key when the caller has not exported it.
if [ -z "${ROUTER_API_KEY:-}" ] && [ -f ./.env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

auth_header=
if [ -n "${ROUTER_API_KEY:-}" ]; then
  auth_header="Authorization: Bearer ${ROUTER_API_KEY}"
fi

curl -fsS --max-time 10 "${base_url}/health" >/dev/null
if [ -n "$auth_header" ]; then
  curl -fsS --max-time 90 -H "$auth_header" -H 'Content-Type: application/json' \
    --data '{"query":"Hermes Agent"}' "${base_url}/v2/search" >/dev/null
else
  curl -fsS --max-time 90 -H 'Content-Type: application/json' \
    --data '{"query":"Hermes Agent"}' "${base_url}/v2/search" >/dev/null
fi
printf '%s\n' "smoke test passed: ${base_url}"

