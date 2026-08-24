#!/bin/sh
set -eu

if ! command -v docker >/dev/null 2>&1; then
  printf '%s\n' "docker is required" >&2
  exit 1
fi

for image in \
  'ghcr.io/redf0x1/camofox-browser@sha256:41e79fb61d50f0a8292b2a51c81ebcb0a2be24d89e9eac970edd12613006ced7' \
  'lightpanda/browser@sha256:b4f155389e172bbc82c3dcbc2282e64db3e2160b27871ef1c53dbc28f7e96887'
do
  docker buildx imagetools inspect "$image" >/dev/null
  printf '%s\n' "available: $image"
done

if [ -f .env ]; then
  crw_image=$(sed -n 's/^CRW_IMAGE=//p' .env | tail -n 1)
  if [ -n "$crw_image" ]; then
    docker buildx imagetools inspect "$crw_image" >/dev/null
    printf '%s\n' "available: $crw_image"
  fi
fi
printf '%s\n' "Pins are reachable. Review upstream release notes before changing them."

