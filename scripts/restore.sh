#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
repo_dir=$(CDPATH= cd -- "$script_dir/.." && pwd -P)
compose() {
  docker compose --project-directory "$repo_dir/dev" -f "$repo_dir/dev/compose.yaml" "$@"
}

if [ "$#" -ne 2 ] || [ "$1" != "--force" ] || [ ! -f "$2" ]; then
  printf '%s\n' "usage: $0 --force BACKUP.tar.gz" >&2
  exit 2
fi
archive_dir=$(cd -- "$(dirname -- "$2")" && pwd -P)
archive_path="${archive_dir}/$(basename -- "$2")"
project=${COMPOSE_PROJECT_NAME:-web-retrieval}
image=alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1

compose down
docker run --rm \
  -i \
  --mount "type=volume,src=${project}_router-data,dst=/target/source" \
  --mount "type=volume,src=${project}_camofox-profiles,dst=/target/profiles" \
  "$image" sh -eu -c '
    find /target/source /target/profiles -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
    tar -xzf - -C /target
  ' <"$archive_path"
printf '%s\n' "restore complete; run: make up"
