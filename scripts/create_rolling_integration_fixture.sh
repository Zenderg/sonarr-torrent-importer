#!/bin/sh
set -eu

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fixture_root=${INTEGRATION_FIXTURE_ROOT:-"$repository_root/.integration-fixtures"}
release_name=${INTEGRATION_RELEASE_NAME:-Futurama.Importer.E2E.720p.WEB-DL-GROUP}
downloads_root="$fixture_root/downloads/$release_name"
revision_one="$fixture_root/rolling-source-r1/$release_name"
revision_two="$fixture_root/rolling-source-r2/$release_name"

mkdir -p "$downloads_root" "$revision_one" "$revision_two"

current_media="$downloads_root/[01].mkv"
if [ ! -f "$current_media" ]; then
  current_media=$(find "$downloads_root" -maxdepth 1 -type f -name '*.S01E01.*.mkv' -print -quit)
fi
if [ -z "$current_media" ] || [ ! -f "$current_media" ]; then
  current_media="$downloads_root/[01].mkv"
  ffmpeg -hide_banner -loglevel error -y \
    -f lavfi -i "testsrc=size=1280x720:rate=1" \
    -f lavfi -i "sine=frequency=1000:sample_rate=48000" \
    -t 301 -c:v mpeg4 -q:v 5 -c:a aac \
    -metadata title="Sonarr rolling integration episode 1" \
    "$current_media"
fi
cp "$current_media" "$revision_one/[01].mkv"
cp "$current_media" "$revision_two/[01].mkv"

ffmpeg -hide_banner -loglevel error -y \
  -f lavfi -i "testsrc2=size=1280x720:rate=1" \
  -f lavfi -i "sine=frequency=1200:sample_rate=48000" \
  -t 301 -c:v mpeg4 -q:v 5 -c:a aac \
  -metadata title="Sonarr rolling integration episode 2" \
  "$revision_two/[02].mkv"

revision_one_hash=$(python3 "$repository_root/scripts/create_rolling_torrent_fixture.py" \
  "$revision_one" "$fixture_root/rolling-rev1.torrent" "http://fixture-server:8080/rolling-source-r1/"
)
revision_two_hash=$(python3 "$repository_root/scripts/create_rolling_torrent_fixture.py" \
  "$revision_two" "$fixture_root/rolling-rev2.torrent" "http://fixture-server:8080/rolling-source-r2/"
)
printf '%s %s\n' "$revision_one_hash" "$revision_two_hash"
