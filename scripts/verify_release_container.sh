#!/bin/sh
set -eu

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
project_name="sonarr-importer-release-check-$$"
image="ghcr.io/zenderg/sonarr-torrent-importer:${IMPORTER_VERSION:-release-smoke}"
invalid_log=$(mktemp)

export IMPORTER_ENV_FILE="${IMPORTER_ENV_FILE:-.env.example}"
export IMPORTER_VERSION="${IMPORTER_VERSION:-release-smoke}"
export IMPORTER_HOST_PORT="${IMPORTER_HOST_PORT:-0}"
export BUILD_VERSION="${BUILD_VERSION:-release-smoke}"
export BUILD_COMMIT_SHA="${BUILD_COMMIT_SHA:-release-check}"
export BUILD_TIME="${BUILD_TIME:-1970-01-01T00:00:00Z}"

compose() {
  docker compose \
    --project-name "$project_name" \
    -f "$repository_root/compose.example.yaml" \
    -f "$repository_root/compose.dev.yaml" \
    "$@"
}

cleanup() {
  compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -f "$invalid_log"
}
trap cleanup EXIT INT TERM

cd "$repository_root"
compose config --quiet
compose build

test "$(docker image inspect --format '{{.Config.User}}' "$image")" = "10001:10001"
test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$image")" = "$BUILD_VERSION"
test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$image")" = "$BUILD_COMMIT_SHA"

compose run --rm --no-deps importer version | grep -F "sonarr-torrent-importer $BUILD_VERSION"

if compose run --rm --no-deps \
  -e SONARR_URL= \
  -e SONARR_API_KEY= \
  -e QBITTORRENT_URL= \
  -e QBITTORRENT_USERNAME= \
  -e QBITTORRENT_PASSWORD= \
  importer serve >"$invalid_log" 2>&1; then
  echo "container accepted invalid configuration" >&2
  exit 1
fi
grep -F "invalid configuration: SONARR_URL is required" "$invalid_log"

compose up -d
attempt=0
until compose exec -T importer wget -q -O - http://127.0.0.1:8080/health | grep -F '"status":"ok"'; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then
    compose logs importer >&2
    exit 1
  fi
  sleep 1
done

test "$(compose exec -T importer id -u)" = "10001"
compose exec -T importer sh -eu -c 'test ! -e /.env; test ! -e /src; test ! -e /.git'
