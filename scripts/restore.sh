#!/bin/sh
set -eu

if [ "$#" -ne 2 ] || [ "$1" != "--force" ] || [ ! -f "$2" ]; then
  printf '%s\n' "usage: $0 --force BACKUP.tar.gz" >&2
  exit 2
fi
archive_dir=$(cd -- "$(dirname -- "$2")" && pwd -P)
archive_name=$(basename -- "$2")
project=${COMPOSE_PROJECT_NAME:-web-retrieval}
image=alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1

docker compose down
docker run --rm \
  --mount "type=volume,src=${project}_router-data,dst=/target/source" \
  --mount "type=volume,src=${project}_camofox-profiles,dst=/target/profiles" \
  --mount "type=bind,src=${archive_dir},dst=/backup,readonly" \
  "$image" sh -eu -c '
    find /target/source /target/profiles -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
    tar -xzf "/backup/$1" -C /target
  ' sh "$archive_name"
printf '%s\n' "restore complete; run: docker compose up -d"

