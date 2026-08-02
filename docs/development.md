# Development

This document is the source of truth for the implementation stack, supported integration boundary, local checks, and container development workflow. Product motivation belongs in [`project-context.md`](project-context.md), evidence-based scope decisions belong in [`concept-review.md`](concept-review.md), and publishing rules belong in [`releases.md`](releases.md).

## Stack and support boundary

- Go 1.21 language compatibility; the container build uses the current Go 1.25 toolchain.
- Standard library only at runtime.
- Sonarr v4 through `/api/v3`.
- qBittorrent v5 with Web API v2.11.0 or newer.
- Rolling releases require qBittorrent Web API v2.14.0 or newer, a libtorrent 2.x build, Prowlarr, and shared media storage.
- Docker Compose is the supported application startup path.

Sonarr v3 has a different manual-import wire format (`language` instead of `languages`, and no v4 release fields). The importer rejects it explicitly instead of maintaining an unverified compatibility path.

## Checks

Run formatting and static checks locally:

```bash
gofmt -w cmd internal
go test ./...
go vet ./...
```

On this development host the preinstalled Go toolchain is incomplete. The Docker build is the authoritative check because it runs `go test ./...`, `go vet ./...`, and the production binary build in the pinned Linux toolchain:

```bash
docker build -t sonarr-torrent-importer:test .
```

Build the same validated artifact used for deployment:

```bash
docker compose -f compose.example.yaml -f compose.dev.yaml build
./scripts/verify_release_container.sh
```

Application startup must remain inside Docker Compose. Direct local commands are appropriate only for formatting, tests, static checks, and one-off scripts.

## Local integration stack

The production Compose example contains only the importer. For local API and workflow development, add the integration override, which starts Sonarr and qBittorrent containers pinned by both tag and digest on the same Docker network:

```bash
cp integration.env.example .env
docker compose -f compose.example.yaml -f compose.dev.yaml -f compose.integration.yaml up -d --build
```

`integration.env.example` contains intentionally public, local-only credentials that match the bootstrap configuration. Do not reuse them outside this loopback-only stack. The repository-local development defaults use importer port `18080`, Sonarr port `18989`, and qBittorrent port `18081`, all bound to `127.0.0.1`. The development override builds `sonarr-torrent-importer:dev`; it never reuses or overwrites the immutable-looking `ghcr.io/...:v1.1.0` reference from the production example. The containers share the `integration-media` named volume at `/media`; qBittorrent downloads to `/media/downloads`, while Sonarr can import into a root folder under `/media/library`. Configuration and media volumes persist across ordinary restarts.

Use the same file list to inspect or stop the stack:

```bash
docker compose -f compose.example.yaml -f compose.dev.yaml -f compose.integration.yaml ps
docker compose -f compose.example.yaml -f compose.dev.yaml -f compose.integration.yaml down
```

Adding `-v` to `down` deletes the local Sonarr, qBittorrent, importer, and media state. The integration override is development-only: it does not change the production image or the CI container smoke test. It provides the services and shared path namespace required for a real workflow fixture, but it does not add tracker, indexer, or personal media credentials.

The complete release-path E2E is automated:

```bash
./scripts/run_integration_e2e.sh
./scripts/run_rolling_integration_e2e.sh
```

The script requires a fresh Compose project. To run another isolated copy without deleting earlier evidence, choose new loopback ports and a project name:

```bash
INTEGRATION_COMPOSE_PROJECT=sonarr-importer-e2e-2 \
IMPORTER_HOST_PORT=28080 \
SONARR_HOST_PORT=28989 \
QBITTORRENT_HOST_PORT=28081 \
INTEGRATION_RELEASE_NAME=Futurama.Importer.E2E2.720p.WEB-DL-GROUP \
./scripts/run_integration_e2e.sh
```

The fixture is intentionally a real 301-second MKV named `[01].mkv` inside a multi-file torrent. The pushed Sonarr release carries the trusted series, season, episode, language, and quality context. A fixture proxy delays the successful qBittorrent rename response long enough to stop the importer in durable phase `rename_file_submitting`; the script then recreates the importer, repeats execute with the same token, and asserts that the proxy observed exactly one rename request. It also asserts the canonical manifest, exact Sonarr import history and episode file, `tv-sonarr-imported` category, queue finalization, and active seeding.

The rolling E2E uses the completed base operation as trusted enrollment evidence, then exposes two revisions through a deterministic Prowlarr fixture. Revision two appends `[02].mkv` and is fetched by qBittorrent from an HTTP webseed. The script restarts the importer after the durable add intent and again after the durable delete intent while each qBittorrent postcondition already exists. It verifies exactly one add and delete request, non-zero verified piece reuse, copy-mode import of only E02, unchanged E01 Sonarr ownership, full post-import recheck, exact old source size/SHA-256 retention, and active seeding of the new canonical revision.

The live Sonarr contract has two important details for a torrent added directly by the rolling engine. Candidate discovery calls `GET /api/v3/manualimport` with the exact folder and without `downloadId`; passing an unknown hash yields an empty candidate list. The importer still attaches the new hash to the explicit reprocess and ManualImport payload, but Sonarr v4 omits `downloadId` from the resulting untracked ManualImport history record. The durable receipt therefore requires a post-baseline `downloadFolderImported` event with the exact hash-scoped staging path, episode, series, file ID and imported path, plus the accepted command and current episode-file metadata. A non-empty history `downloadId` is accepted only when it equals the candidate hash. The rolling E2E is the source of truth for this behavior.

Container bits are reproducible by digest, but the E2E is intentionally not hermetic: Sonarr resolves `tvdb:73871` and refreshes series metadata through its normal external metadata services. Run it with internet access; an upstream metadata outage is distinguishable from a container or importer failure in the Sonarr logs.

## Implementation layout

- `cmd/importer`: CLI and HTTP service process.
- `internal/sonarr`: Sonarr v4 API adapter.
- `internal/qbittorrent`: authenticated manifest and file-rename adapter.
- `internal/prowlarr`: exact release search and credential-contained torrent metadata fetch.
- `internal/metainfo`: bounded strict v1/v2/hybrid torrent metadata parsing and identity.
- `internal/mapper`: pure deterministic `[NN].mkv` episode mapper.
- `internal/workflow`: dry-run, durable rename reconciliation, Sonarr auto/manual import, verification, and queue finalization.
- `internal/rolling`: enrollment, immutable artifacts, isolated staging, safe reuse, revision reconciliation, Sonarr receipts, and retirement.
- `internal/server`: health, status, dry-run, and execute HTTP endpoints.

The service uses a global advisory execute lock plus atomically replaced JSON operation records under `/data/operations`. The latest persisted result is restored by the status API after restart, and the one-shot CLI emits the full JSON result.

An operation is written as `prepared` before mutation, `rename_file_submitting` before each qBittorrent rename, `renames_verified` after the canonical manifest is proven, `manual_import_submitting` before an explicit Sonarr command, and `queue_finalizing` before each queue DELETE. qBittorrent rename is asynchronous even after HTTP 200, so file-index manifest observation is the proof of completion. If a response is lost or the process restarts, the same `downloadId` plus dry-run `planToken` resumes postcondition reconciliation without blindly repeating an uncertain mutation.

The `/data` safety contract assumes a local filesystem with advisory `flock`, atomic same-directory rename, and `fsync` semantics. Docker named volumes and local bind mounts are supported; NFS, CIFS, and object-backed mounts are not supported. Source content must be regular qBittorrent-managed torrent content; symlinked content trees are outside the supported security boundary because the Web API does not expose file type information. qBittorrent and the storage administrator are trusted writers: no other process may concurrently replace directory topology or introduce symlinks beneath the configured media root while a rolling operation is active.

Rolling state is stored separately under `/data/rolling/releases` and exact source metadata under `/data/rolling/artifacts`. It shares `/data/execute.lock` with the normal importer. Rolling media access is opt-in through `compose.rolling.example.yaml`: the importer gets the qBittorrent storage as a read/write bind mount plus the storage GID. `QBITTORRENT_MEDIA_ROOT`, `SONARR_MEDIA_ROOT`, and `IMPORTER_MEDIA_ROOT` explicitly describe the three container path namespaces for that same storage. Every candidate uses a release/hash-specific isolated directory, rejects symlinks and non-regular reuse sources, removes interrupted temporary copies, copies with cancellation into an independent inode, fsyncs the copy, forces qBittorrent recheck, and compares SHA-256 again after Sonarr copy-import. Old files are deliberately not cleaned up automatically.
