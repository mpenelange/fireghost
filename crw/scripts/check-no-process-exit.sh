#!/usr/bin/env bash
# Mechanical guard against the browser-orphan leak class.
#
# Any browser-spawning command MUST return its failure as a value and let
# the single consolidated exit path (teardown::finish) run
# kill_all_browsers() before the process dies. A direct exit-from-libc call
# after a browser was spawned bypasses Drop and orphans LightPanda/Chrome
# process groups.
#
# This script scans every CLI/MCP source file that touches the browser
# spawn/guard surface (plus the two binary entrypoints) and fails if it
# finds a direct exit/abort call that is not explicitly annotated with the
# line-level marker `teardown-exit-ok`.
#
# Portable to macOS bash 3.2: no mapfile/readarray, no associative arrays.

set -euo pipefail

cd "$(dirname "$0")/.."

# Symbols that prove a file participates in browser spawning / guarding.
SPAWN_SYMBOLS='spawn_headless|spawn_all_headless|ManagedBrowser|keep_alive_guards'

# The two binary entrypoints route every command result through the
# consolidated exit; they are always in scope even if they never name a
# spawn symbol directly.
ENTRYPOINTS="crates/crw-cli/src/main.rs crates/crw-mcp/src/main.rs"

# Match a direct exit/abort call. Written as an alternation so this script
# never contains the exact token it forbids (and so it never flags itself).
EXIT_RE='process::(exit|abort)[[:space:]]*\('

# Allow-list marker. A flagged line is permitted only if the SAME line also
# carries this token (a trailing `// teardown-exit-ok` comment).
MARKER='teardown-exit-ok'

# Build the scan set: spawn/guard files UNION the entrypoints, deduped.
scan_set=$(
  {
    grep -rlE "$SPAWN_SYMBOLS" crates/crw-cli/src crates/crw-mcp/src || true
    for f in $ENTRYPOINTS; do
      echo "$f"
    done
  } | sort -u
)

violations=0

for file in $scan_set; do
  [ -f "$file" ] || continue
  # `grep -nE` lists `lineno:content`; keep only lines lacking the marker.
  while IFS= read -r hit; do
    [ -n "$hit" ] || continue
    case "$hit" in
      *"$MARKER"*) ;;  # explicitly allowed
      *)
        if [ "$violations" -eq 0 ]; then
          echo "error: direct exit/abort after a browser may have spawned." >&2
          echo "       Return Err(CmdError) and let teardown::finish run instead," >&2
          echo "       or annotate the line with a trailing // ${MARKER} comment" >&2
          echo "       if the call site provably owns no browser." >&2
          echo >&2
        fi
        echo "  ${file}:${hit}" >&2
        violations=$((violations + 1))
        ;;
    esac
  done < <(grep -nE "$EXIT_RE" "$file" || true)
done

if [ "$violations" -gt 0 ]; then
  echo >&2
  echo "FAIL: ${violations} unguarded exit call(s) in browser-spawning code." >&2
  exit 1
fi

echo "ok: no unguarded exit calls in browser-spawning code"
