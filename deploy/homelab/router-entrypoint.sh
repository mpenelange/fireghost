#!/bin/sh
set -eu
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
