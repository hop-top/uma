#!/usr/bin/env bash
# Removes what up.sh created: only containers whose name starts with
# uma-live-evidence- AND that carry the label uma.live-evidence=1. With
# UMA_LIVE_RUN set, only that run's. It never touches any other container,
# volume, network or image. --images also removes the two images up.sh built.
set -euo pipefail

prefix=uma-live-evidence
filter=(--filter "label=uma.live-evidence=1" --filter "name=^$prefix-")
[ -z "${UMA_LIVE_RUN:-}" ] || filter+=(--filter "label=uma.live-evidence.run=$UMA_LIVE_RUN")

ids=$(docker ps -a -q "${filter[@]}")
if [ -n "$ids" ]; then
  # shellcheck disable=SC2086
  docker rm -f -v $ids >/dev/null
  echo "down: removed $(echo "$ids" | wc -l | tr -d ' ') container(s)" >&2
fi

if [ "${1:-}" = "--images" ]; then
  docker image rm "$prefix-dovecot:1" "$prefix-postfix:1" >/dev/null 2>&1 || true
fi

state=${UMA_LIVE_STATE:-}
case "$state" in
  */$prefix-*) rm -rf "$state" ;;
esac
