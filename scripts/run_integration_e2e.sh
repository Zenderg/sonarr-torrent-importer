#!/bin/sh
set -eu

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose_project=${INTEGRATION_COMPOSE_PROJECT:-sonarr-torrent-importer-e2e}
fixture_host_path=${INTEGRATION_FIXTURE_HOST_PATH:-"$repository_root/.integration-fixtures/$compose_project"}
importer_port=${IMPORTER_HOST_PORT:-28080}
sonarr_port=${SONARR_HOST_PORT:-28989}
qbittorrent_port=${QBITTORRENT_HOST_PORT:-28081}
sonarr_api_key=${SONARR_API_KEY:-0123456789abcdef0123456789abcdef}
qbittorrent_username=${QBITTORRENT_USERNAME:-importer}
qbittorrent_password=${QBITTORRENT_PASSWORD:-importer-dev-qbit-password}
release_name=${INTEGRATION_RELEASE_NAME:-Futurama.Importer.E2E.720p.WEB-DL-GROUP}
release_title="Futurama.S01E01.Integration.720p.WEB-DL-GROUP"
publish_date=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
canonical_relative_path="$release_name/Futurama.S01E01.WEBDL-720p.mkv"
sonarr_url="http://127.0.0.1:$sonarr_port"
importer_url="http://127.0.0.1:$importer_port"
qbittorrent_url="http://127.0.0.1:$qbittorrent_port"
cookie_file=$(mktemp "${TMPDIR:-/tmp}/sonarr-importer-qbit.XXXXXX")
execute_file=$(mktemp "${TMPDIR:-/tmp}/sonarr-importer-execute.XXXXXX")
execute_error_file=$(mktemp "${TMPDIR:-/tmp}/sonarr-importer-execute-error.XXXXXX")
execute_pid=

cleanup() {
  if [ -n "$execute_pid" ]; then
    kill "$execute_pid" >/dev/null 2>&1 || true
  fi
  rm -f "$cookie_file" "$execute_file" "$execute_error_file"
}
trap cleanup EXIT HUP INT TERM

export IMPORTER_HOST_PORT="$importer_port"
export IMPORTER_ENV_FILE="${IMPORTER_ENV_FILE:-integration.env.example}"
export SONARR_HOST_PORT="$sonarr_port"
export QBITTORRENT_HOST_PORT="$qbittorrent_port"
export SONARR_API_KEY="$sonarr_api_key"
export QBITTORRENT_USERNAME="$qbittorrent_username"
export QBITTORRENT_PASSWORD="$qbittorrent_password"
export INTEGRATION_FIXTURE_HOST_PATH="$fixture_host_path"
export INTEGRATION_FIXTURE_ROOT="$fixture_host_path"

integration_compose() {
  docker compose \
    -p "$compose_project" \
    -f compose.example.yaml \
    -f compose.dev.yaml \
    -f compose.integration.yaml \
    "$@"
}

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "$1 is required" >&2
    exit 1
  fi
}

wait_for_url() {
  url=$1
  attempts=0
  until curl -fsS "$url" >/dev/null 2>&1; do
    attempts=$((attempts + 1))
    if [ "$attempts" -ge 90 ]; then
      echo "timed out waiting for $url" >&2
      exit 1
    fi
    sleep 2
  done
}

require_command curl
require_command docker
require_command ffmpeg
require_command jq
require_command python3

cd "$repository_root"
info_hash=$(INTEGRATION_RELEASE_NAME="$release_name" ./scripts/create_integration_fixture.sh)

integration_compose up -d --build

wait_for_url "$sonarr_url/ping"
wait_for_url "$qbittorrent_url/"
wait_for_url "$importer_url/health"

if [ "$(curl -fsS -H "X-Api-Key: $sonarr_api_key" "$sonarr_url/api/v3/series" | jq 'length')" -ne 0 ]; then
  echo "Sonarr integration state is not empty; use a fresh INTEGRATION_COMPOSE_PROJECT" >&2
  exit 1
fi

curl -fsS -X POST \
  -H "X-Api-Key: $sonarr_api_key" \
  -H 'Content-Type: application/json' \
  -d '{"path":"/media/library"}' \
  "$sonarr_url/api/v3/rootfolder" >/dev/null

curl -fsS -H "X-Api-Key: $sonarr_api_key" "$sonarr_url/api/v3/downloadclient/schema" |
  jq -c --arg username "$qbittorrent_username" --arg password "$qbittorrent_password" '
    .[] | select(.implementation == "QBittorrent") |
    .enable = true |
    .removeCompletedDownloads = false |
    .name = "Local qBittorrent" |
    (.fields[] | select(.name == "host").value) = "qbittorrent" |
    (.fields[] | select(.name == "port").value) = 8080 |
    (.fields[] | select(.name == "username").value) = $username |
    (.fields[] | select(.name == "password").value) = $password |
    (.fields[] | select(.name == "tvCategory").value) = "tv-sonarr" |
    (.fields[] | select(.name == "tvImportedCategory").value) = "tv-sonarr-imported"
  ' |
  curl -fsS -X POST \
    -H "X-Api-Key: $sonarr_api_key" \
    -H 'Content-Type: application/json' \
    -d @- \
    "$sonarr_url/api/v3/downloadclient" >/dev/null

curl -fsS -H "X-Api-Key: $sonarr_api_key" "$sonarr_url/api/v3/series/lookup?term=tvdb%3A73871" |
  jq -c '.[0] |
    .qualityProfileId = 3 |
    .rootFolderPath = "/media/library" |
    .seasonFolder = true |
    .monitored = true |
    .addOptions = {
      "monitor":"firstSeason",
      "searchForMissingEpisodes":false,
      "searchForCutoffUnmetEpisodes":false
    }
  ' |
  curl -fsS -X POST \
    -H "X-Api-Key: $sonarr_api_key" \
    -H 'Content-Type: application/json' \
    -d @- \
    "$sonarr_url/api/v3/series" >/dev/null

curl -fsS -X POST \
  -H "X-Api-Key: $sonarr_api_key" \
  -H 'Content-Type: application/json' \
  -d "{\"title\":\"$release_title\",\"downloadUrl\":\"http://fixture-server:8080/integration.torrent\",\"protocol\":\"torrent\",\"publishDate\":\"$publish_date\",\"indexer\":\"Local integration fixture\",\"downloadClientId\":1}" \
  "$sonarr_url/api/v3/release/push" >/dev/null

attempts=0
while :; do
  queue_download_id=$(curl -fsS -H "X-Api-Key: $sonarr_api_key" "$sonarr_url/api/v3/queue/details" | jq -r '.[0].downloadId // empty')
  if [ "$(printf '%s' "$queue_download_id" | tr '[:upper:]' '[:lower:]')" = "$info_hash" ]; then
    break
  fi
  attempts=$((attempts + 1))
  if [ "$attempts" -ge 60 ]; then
    echo "timed out waiting for Sonarr queue item $info_hash" >&2
    exit 1
  fi
  sleep 2
done

dry_run=$(curl -fsS -X POST \
  -H 'Content-Type: application/json' \
  -d "{\"downloadId\":\"$info_hash\"}" \
  "$importer_url/api/v1/imports/dry-run")
plan_token=$(printf '%s' "$dry_run" | jq -r '.planToken')
printf '%s' "$dry_run" | jq -e \
  --arg target "$canonical_relative_path" \
  '.outcome == "ready" and .canExecute == true and .files[0].rename.toPath == $target' >/dev/null

curl -fsS -c "$cookie_file" \
  -d "username=$qbittorrent_username&password=$qbittorrent_password" \
  "$qbittorrent_url/api/v2/auth/login" >/dev/null

curl -fsS -X POST \
  -H 'Content-Type: application/json' \
  -d "{\"downloadId\":\"$info_hash\",\"confirmDownloadId\":\"$info_hash\",\"planToken\":\"$plan_token\"}" \
  "$importer_url/api/v1/imports/execute" >"$execute_file" 2>"$execute_error_file" &
execute_pid=$!

attempts=0
recovery_phase=
while :; do
  operation=$(integration_compose exec -T importer sh -c 'if [ -f /data/operations/latest.json ]; then cat /data/operations/latest.json; fi' 2>/dev/null || true)
  phase=$(printf '%s' "$operation" | jq -r '.phase // empty')
  qbit_files=$(curl -fsS -b "$cookie_file" "$qbittorrent_url/api/v2/torrents/files?hash=$info_hash")
  if [ "$phase" = "rename_file_submitting" ] && printf '%s' "$qbit_files" | jq -e --arg target "$canonical_relative_path" '.[0].name == $target' >/dev/null; then
    recovery_phase=$phase
    break
  fi
  if ! kill -0 "$execute_pid" 2>/dev/null; then
    execute_status=0
    wait "$execute_pid" || execute_status=$?
    execute_pid=
    echo "execute ended with status $execute_status before the rename recovery point" >&2
    if [ -s "$execute_error_file" ]; then
      echo "execute stderr:" >&2
      sed 's/^/  /' "$execute_error_file" >&2
    fi
    if [ -s "$execute_file" ]; then
      echo "execute response:" >&2
      sed 's/^/  /' "$execute_file" >&2
    fi
    echo "qBittorrent manifest:" >&2
    printf '%s\n' "$qbit_files" | jq >&2
    echo "importer logs:" >&2
    integration_compose logs --no-color importer >&2
    exit 1
  fi
  attempts=$((attempts + 1))
  if [ "$attempts" -ge 600 ]; then
    echo "timed out waiting for a durable rename_file_submitting recovery point; last phase was $phase" >&2
    printf '%s\n' "$qbit_files" | jq >&2
    exit 1
  fi
  sleep 0.1
done

rename_requests=$(integration_compose exec -T fixture-server wget -qO- http://127.0.0.1:8080/metrics | jq -r '.renameRequests')
if [ "$rename_requests" -ne 1 ]; then
  echo "fixture proxy observed $rename_requests rename requests before restart, expected 1" >&2
  exit 1
fi

integration_compose stop -t 0 importer
if wait "$execute_pid"; then
  echo "execute unexpectedly completed before the forced restart" >&2
  exit 1
fi
execute_pid=
integration_compose up -d --no-deps importer
wait_for_url "$importer_url/health"

execute=$(curl -fsS -X POST \
  -H 'Content-Type: application/json' \
  -d "{\"downloadId\":\"$info_hash\",\"confirmDownloadId\":\"$info_hash\",\"planToken\":\"$plan_token\"}" \
  "$importer_url/api/v1/imports/execute")
printf '%s' "$execute" | jq -e '
  .outcome == "imported" and
  .operationPhase == "complete" and
  .files[0].rename.status == "applied" and
  .files[0].verification.historyId > 0 and
  .files[0].verification.episodeFileId > 0 and
  .queueFinalization.status == "verified"
' >/dev/null

rename_requests=$(integration_compose exec -T fixture-server wget -qO- http://127.0.0.1:8080/metrics | jq -r '.renameRequests')
if [ "$rename_requests" -ne 1 ]; then
  echo "rename mutation was repeated during recovery: proxy observed $rename_requests requests" >&2
  exit 1
fi
qbit_files=$(curl -fsS -b "$cookie_file" "$qbittorrent_url/api/v2/torrents/files?hash=$info_hash")
printf '%s' "$qbit_files" | jq -e --arg target "$canonical_relative_path" \
  'length == 1 and .[0].name == $target and .[0].progress == 1 and .[0].priority > 0' >/dev/null
qbit_state=$(curl -fsS -b "$cookie_file" "$qbittorrent_url/api/v2/torrents/info?hashes=$info_hash")
printf '%s' "$qbit_state" | jq -e \
  'length == 1 and .[0].category == "tv-sonarr-imported" and (.[0].state == "uploading" or .[0].state == "stalledUP" or .[0].state == "forcedUP")' >/dev/null

printf '%s\n' "$execute" | jq --arg recoveryPhase "$recovery_phase" --argjson renameRequests "$rename_requests" '. + {e2eRecoveryPhase:$recoveryPhase,e2eRenameRequests:$renameRequests}'
