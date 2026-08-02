#!/bin/sh
set -eu

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fixture_root=${INTEGRATION_FIXTURE_ROOT:-"$repository_root/.integration-fixtures"}
downloads_root="$fixture_root/downloads"
release_name=${INTEGRATION_RELEASE_NAME:-Futurama.Integration.720p.WEB-DL-GROUP}
media_path="$downloads_root/$release_name/[01].mkv"
torrent_path="$fixture_root/integration.torrent"

if ! command -v ffmpeg >/dev/null 2>&1; then
  echo "ffmpeg is required to create the integration media fixture" >&2
  exit 1
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "python3 is required to create the integration torrent fixture" >&2
  exit 1
fi

mkdir -p "$(dirname -- "$media_path")"
ffmpeg \
  -hide_banner \
  -loglevel error \
  -y \
  -f lavfi \
  -i "testsrc=size=1280x720:rate=1" \
  -f lavfi \
  -i "sine=frequency=1000:sample_rate=48000" \
  -t 301 \
  -c:v mpeg4 \
  -q:v 5 \
  -c:a aac \
  -metadata title="Sonarr importer integration fixture" \
  "$media_path"

python3 "$repository_root/scripts/create_torrent_fixture.py" "$media_path" "$torrent_path"
