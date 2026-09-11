#!/bin/sh
set -eu
allowance=${FIRECRAWL_MONTHLY_ALLOWANCE:-1500}
buffer=${FIRECRAWL_BUFFER_PERCENT:-20}
# Canonical decimal integers only; bound arithmetic and reject zero caps.
case "$allowance" in ''|*[!0-9]*|0*) printf '%s\n' 'Invalid monthly allowance' >&2; exit 1;; esac
case "$buffer" in ''|*[!0-9]*) printf '%s\n' 'Invalid buffer percent' >&2; exit 1;; esac
if [ "${#allowance}" -gt 8 ] || [ "${#buffer}" -gt 2 ]; then
  printf '%s\n' 'Budget values out of range' >&2; exit 1
fi
case "$buffer" in 0[0-9]*) printf '%s\n' 'Use canonical decimal buffer percent' >&2; exit 1;; esac
ROUTER_MONTHLY_CLOUD_CREDITS=$((allowance * (100 - buffer) / 100))
if [ "$ROUTER_MONTHLY_CLOUD_CREDITS" -le 0 ]; then
  printf '%s\n' 'Buffered monthly allowance must be positive' >&2; exit 1
fi
export ROUTER_MONTHLY_CLOUD_CREDITS
# No midnight daily quota: persistent token bucket provides smooth pacing.
export ROUTER_DAILY_CLOUD_CREDITS=0
case "${FIRECRAWL_ENABLED:-false}" in
  false) unset FIRECRAWL_CLOUD_API_KEY ;;
  true)
    if [ -z "${FIRECRAWL_CLOUD_API_KEY:-}" ]; then
      printf '%s\n' 'FIRECRAWL_ENABLED=true requires FIRECRAWL_CLOUD_API_KEY' >&2
      exit 1
    fi
    ;;
  *) printf '%s\n' 'FIRECRAWL_ENABLED must be true or false' >&2; exit 1 ;;
esac
if [ "$#" -eq 0 ]; then set -- /usr/local/bin/router; fi
exec "$@"
