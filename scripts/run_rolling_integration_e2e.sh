#!/bin/sh
set -eu

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose_project=${ROLLING_INTEGRATION_COMPOSE_PROJECT:-sonarr-torrent-importer-rolling-e2e}
fixture_host_path=${INTEGRATION_FIXTURE_HOST_PATH:-"$repository_root/.integration-fixtures/$compose_project"}
importer_port=${IMPORTER_HOST_PORT:-38080}
sonarr_port=${SONARR_HOST_PORT:-38989}
qbittorrent_port=${QBITTORRENT_HOST_PORT:-38081}
sonarr_api_key=${SONARR_API_KEY:-0123456789abcdef0123456789abcdef}
qbittorrent_username=${QBITTORRENT_USERNAME:-importer}
qbittorrent_password=${QBITTORRENT_PASSWORD:-importer-dev-qbit-password}
release_name=${INTEGRATION_RELEASE_NAME:-Futurama.Importer.E2E.720p.WEB-DL-GROUP}
importer_url="http://127.0.0.1:$importer_port"
sonarr_url="http://127.0.0.1:$sonarr_port"
qbittorrent_url="http://127.0.0.1:$qbittorrent_port"
cookie_file=$(mktemp "${TMPDIR:-/tmp}/sonarr-importer-rolling-qbit.XXXXXX")
check_file=$(mktemp "${TMPDIR:-/tmp}/sonarr-importer-rolling-check.XXXXXX")
check_pid=

cleanup() {
  if [ -n "$check_pid" ]; then
    kill "$check_pid" >/dev/null 2>&1 || true
  fi
  rm -f "$cookie_file" "$check_file"
}
trap cleanup EXIT HUP INT TERM

export INTEGRATION_COMPOSE_PROJECT="$compose_project"
export IMPORTER_HOST_PORT="$importer_port"
export SONARR_HOST_PORT="$sonarr_port"
export QBITTORRENT_HOST_PORT="$qbittorrent_port"
export INTEGRATION_FIXTURE_HOST_PATH="$fixture_host_path"
export INTEGRATION_FIXTURE_ROOT="$fixture_host_path"
export QBIT_ADD_RESPONSE_DELAY=${QBIT_ADD_RESPONSE_DELAY:-5s}
export QBIT_DELETE_RESPONSE_DELAY=${QBIT_DELETE_RESPONSE_DELAY:-5s}

integration_compose() {
  docker compose -p "$compose_project" -f compose.example.yaml -f compose.dev.yaml -f compose.integration.yaml "$@"
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

cd "$repository_root"
./scripts/run_integration_e2e.sh >/dev/null

set -- $(./scripts/create_rolling_integration_fixture.sh)
revision_one_hash=$1
revision_two_hash=$2

current_operation=$(integration_compose exec -T importer sed -n '1,$p' /data/operations/latest.json)
current_hash=$(printf '%s' "$current_operation" | jq -r '.plan.context.downloadId | ascii_downcase')
if [ "$revision_one_hash" != "$current_hash" ] || [ "$(printf '%s' "$current_operation" | jq -r '.phase')" != "complete" ]; then
  echo "rolling revision one does not match the completed base import" >&2
  exit 1
fi

episode_one_file_id=$(curl -fsS -H "X-Api-Key: $sonarr_api_key" "$sonarr_url/api/v3/episode?seriesId=1&seasonNumber=1&includeEpisodeFile=true" | jq -r '.[] | select(.episodeNumber == 1).episodeFileId')
old_source="/media/downloads/$release_name/Futurama.S01E01.WEBDL-720p.mkv"
old_source_digest=$(integration_compose exec -T importer sha256sum "$old_source" | awk '{print $1}')
old_source_size=$(integration_compose exec -T importer wc -c "$old_source" | awk '{print $1}')

integration_compose up -d --build --force-recreate fixture-server importer
wait_for_url "$importer_url/health"

enrollment=$(curl -fsS -X POST -H 'Content-Type: application/json' \
  -d "{\"releaseId\":\"futurama-s01\",\"downloadId\":\"$revision_one_hash\",\"confirmDownloadId\":\"$revision_one_hash\",\"indexerId\":1,\"guid\":\"fixture:rolling-futurama-s01\",\"query\":\"Futurama S01\"}" \
  "$importer_url/api/v1/rolling-releases")
printf '%s' "$enrollment" | jq -e --arg hash "$revision_one_hash" '.status == "current" and .currentRevision.torrentId == $hash' >/dev/null

integration_compose exec -T fixture-server wget -qO- --post-data='' http://127.0.0.1:8080/advance >/dev/null

curl -fsS -X POST -H 'Content-Type: application/json' -d '{"releaseId":"futurama-s01"}' \
  "$importer_url/api/v1/rolling-releases/check" >"$check_file" &
check_pid=$!

curl -fsS -c "$cookie_file" -d "username=$qbittorrent_username&password=$qbittorrent_password" "$qbittorrent_url/api/v2/auth/login" >/dev/null
attempts=0
while :; do
  state=$(integration_compose exec -T importer sed -n '1,$p' /data/rolling/releases/futurama-s01.json 2>/dev/null || true)
  phase=$(printf '%s' "$state" | jq -r '.operation.phase // empty')
  candidate=$(curl -fsS -b "$cookie_file" "$qbittorrent_url/api/v2/torrents/info?hashes=$revision_two_hash")
  if [ "$phase" = "new_add_submitting" ] && printf '%s' "$candidate" | jq -e 'length == 1' >/dev/null; then
    break
  fi
  attempts=$((attempts + 1))
  if [ "$attempts" -ge 120 ]; then
    echo "timed out waiting for durable rolling add recovery point; last phase was $phase" >&2
    exit 1
  fi
  sleep 0.1
done

integration_compose stop -t 0 importer
wait "$check_pid" >/dev/null 2>&1 || true
check_pid=
integration_compose up -d --no-deps importer
wait_for_url "$importer_url/health"

attempts=0
while :; do
	state=$(integration_compose exec -T importer sed -n '1,$p' /data/rolling/releases/futurama-s01.json 2>/dev/null || true)
	phase=$(printf '%s' "$state" | jq -r '.operation.phase // empty')
	status=$(printf '%s' "$state" | jq -r '.status // empty')
	old_torrent=$(curl -fsS -b "$cookie_file" "$qbittorrent_url/api/v2/torrents/info?hashes=$revision_one_hash")
	if [ "$phase" = "old_delete_submitting" ] && printf '%s' "$old_torrent" | jq -e 'length == 0' >/dev/null; then
		break
	fi
	if [ "$status" = "blocked" ]; then
		printf '%s\n' "$state" | jq >&2
		exit 1
	fi
	attempts=$((attempts + 1))
	if [ "$attempts" -ge 600 ]; then
		echo "timed out waiting for durable rolling delete recovery point; last phase was $phase" >&2
		exit 1
	fi
	sleep 0.1
done

integration_compose stop -t 0 importer
integration_compose up -d --no-deps importer
wait_for_url "$importer_url/health"

attempts=0
while :; do
  release=$(curl -fsS "$importer_url/api/v1/rolling-releases/futurama-s01")
  status=$(printf '%s' "$release" | jq -r '.status')
  current=$(printf '%s' "$release" | jq -r '.currentRevision.torrentId')
  if [ "$status" = "current" ] && [ "$current" = "$revision_two_hash" ]; then
    break
  fi
  if [ "$status" = "blocked" ]; then
    printf '%s\n' "$release" | jq >&2
    exit 1
  fi
  attempts=$((attempts + 1))
  if [ "$attempts" -ge 300 ]; then
    echo "timed out waiting for rolling revision completion; status=$status phase=$(printf '%s' "$release" | jq -r '.operation.phase // empty')" >&2
    exit 1
  fi
  sleep 2
done

add_requests=$(integration_compose exec -T fixture-server wget -qO- http://127.0.0.1:8080/metrics | jq -r '.addRequests')
if [ "$add_requests" -ne 1 ]; then
  echo "candidate add was repeated after restart: $add_requests requests" >&2
  exit 1
fi
delete_requests=$(integration_compose exec -T fixture-server wget -qO- http://127.0.0.1:8080/metrics | jq -r '.deleteRequests')
if [ "$delete_requests" -ne 1 ]; then
	echo "old torrent deletion was repeated after restart: $delete_requests requests" >&2
	exit 1
fi

printf '%s' "$release" | jq -e '
  .currentRevision.reusedBytes > 0 and
  .currentRevision.reusedBytes < .currentRevision.totalLength
' >/dev/null

old_torrent=$(curl -fsS -b "$cookie_file" "$qbittorrent_url/api/v2/torrents/info?hashes=$revision_one_hash")
new_torrent=$(curl -fsS -b "$cookie_file" "$qbittorrent_url/api/v2/torrents/info?hashes=$revision_two_hash")
new_files=$(curl -fsS -b "$cookie_file" "$qbittorrent_url/api/v2/torrents/files?hash=$revision_two_hash")
printf '%s' "$old_torrent" | jq -e 'length == 0' >/dev/null
printf '%s' "$new_torrent" | jq -e 'length == 1 and .[0].progress == 1 and (.[0].state == "uploading" or .[0].state == "stalledUP" or .[0].state == "forcedUP")' >/dev/null
printf '%s' "$new_files" | jq -e '
  length == 2 and
  .[0].name == "Futurama.Importer.E2E.720p.WEB-DL-GROUP/Futurama.S01E01.WEBDL-720p.mkv" and
  .[1].name == "Futurama.Importer.E2E.720p.WEB-DL-GROUP/Futurama.S01E02.WEBDL-720p.mkv" and
  all(.[]; .progress == 1 and .priority > 0)
' >/dev/null

episodes=$(curl -fsS -H "X-Api-Key: $sonarr_api_key" "$sonarr_url/api/v3/episode?seriesId=1&seasonNumber=1&includeEpisodeFile=true")
printf '%s' "$episodes" | jq -e --argjson oldFile "$episode_one_file_id" '
  (.[] | select(.episodeNumber == 1) | .episodeFileId) == $oldFile and
  (.[] | select(.episodeNumber == 2) |
    .hasFile and .episodeFileId > 0 and
    (.episodeFile.path | endswith("/Futurama.S01E02.WEBDL-720p.mkv")))
' >/dev/null

old_source_digest_after=$(integration_compose exec -T importer sha256sum "$old_source" | awk '{print $1}')
old_source_size_after=$(integration_compose exec -T importer wc -c "$old_source" | awk '{print $1}')
if [ "$old_source_digest_after" != "$old_source_digest" ] || [ "$old_source_size_after" != "$old_source_size" ]; then
	echo "old source content changed during keep-content retirement" >&2
	exit 1
fi

new_episode_one_source=$(printf '%s' "$release" | jq -r '.currentRevision.savePath + "/" + (.currentRevision.files[] | select(.episodeNumber == 1).currentPath)')
new_episode_two_source=$(printf '%s' "$release" | jq -r '.currentRevision.savePath + "/" + (.currentRevision.files[] | select(.episodeNumber == 2).currentPath)')
integration_compose exec -T importer test -f "$new_episode_one_source"
integration_compose exec -T importer test -f "$new_episode_two_source"

printf '%s\n' "$release" | jq \
  --argjson addRequests "$add_requests" \
  --argjson deleteRequests "$delete_requests" \
  '. + {e2eAddRequests:$addRequests,e2eDeleteRequests:$deleteRequests}'
