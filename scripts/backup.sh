#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
repo_dir=$(CDPATH= cd -- "$script_dir/.." && pwd -P)
compose() {
  docker compose --project-directory "$repo_dir/deploy" -f "$repo_dir/deploy/compose.yaml" "$@"
}

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
  printf '%s\n' "usage: $0 BACKUP.tar.gz" >&2
  exit 2
fi
case "$1" in */) printf '%s\n' "backup path must name a file" >&2; exit 2 ;; esac

archive_dir=$(dirname -- "$1")
archive_name=$(basename -- "$1")
mkdir -p -- "$archive_dir"
archive_dir=$(cd -- "$archive_dir" && pwd -P)
project=${COMPOSE_PROJECT_NAME:-web-retrieval}
image=alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1

# Firefox profiles and the credit ledger must be quiescent for a consistent
# archive. Restart only services that were running when the backup began.
running_services=$(compose ps --status running --services | xargs)
restart_services() {
  if [ -n "$running_services" ]; then
    # Service names come from this repository's Compose model.
    # shellcheck disable=SC2086
    compose start $running_services >/dev/null
  fi
}
if [ -n "$running_services" ]; then
  # shellcheck disable=SC2086
  compose stop $running_services >/dev/null
  trap restart_services EXIT HUP INT TERM
fi

docker run --rm \
  --mount "type=volume,src=${project}_router-data,dst=/source,readonly" \
  --mount "type=volume,src=${project}_camofox-profiles,dst=/profiles,readonly" \
  --mount "type=bind,src=${archive_dir},dst=/backup" \
  "$image" tar -czf "/backup/${archive_name}" -C / source profiles
restart_services
trap - EXIT HUP INT TERM
printf '%s\n' "backup written to ${archive_dir}/${archive_name}"
