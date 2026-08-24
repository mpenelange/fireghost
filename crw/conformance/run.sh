#!/usr/bin/env bash
# Firecrawl v2 conformance runner. Honors repo tooling (uv).
#
#   ./run.sh capture   # capture golden fixtures from the real api.firecrawl.dev
#   ./run.sh compare   # diff crw's responses against the golden fixtures
#   ./run.sh sdk       # run the real firecrawl-py SDK against crw (issue #62)
#   ./run.sh all       # compare + sdk (the CI gate)
#
# Env: FIRECRAWL_API_KEY (capture), CRW_URL / CRW_API_KEY (compare, sdk).
set -euo pipefail
cd "$(dirname "$0")"

case "${1:-all}" in
  capture) uv run python -m conformance.capture ;;
  compare) uv run python -m conformance.compare ;;
  sdk)     uv run python test_sdk.py ;;
  all)     uv run python -m conformance.compare && uv run python test_sdk.py ;;
  *) echo "usage: run.sh [capture|compare|sdk|all]"; exit 1 ;;
esac
