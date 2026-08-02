# Development

This document is the source of truth for the implementation stack, supported integration boundary, local checks, and container development workflow. Product motivation belongs in [`project-context.md`](project-context.md), evidence-based scope decisions belong in [`concept-review.md`](concept-review.md), and publishing rules belong in [`releases.md`](releases.md).

## Stack and support boundary

- Go 1.21 language compatibility; the container build uses the current Go 1.25 toolchain.
- Standard library only at runtime.
- Sonarr v4 through `/api/v3`.
- qBittorrent v5 with Web API v2.11.0 or newer.
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

`integration.env.example` contains intentionally public, local-only credentials that match the bootstrap configuration. Do not reuse them outside this loopback-only stack. The repository-local development defaults use importer port `18080`, Sonarr port `18989`, and qBittorrent port `18081`, all bound to `127.0.0.1`. The development override builds `sonarr-torrent-importer:dev`; it never reuses or overwrites the immutable-looking `ghcr.io/...:v1.0.0` reference from the production example. The containers share the `integration-media` named volume at `/media`; qBittorrent downloads to `/media/downloads`, while Sonarr can import into a root folder under `/media/library`. Configuration and media volumes persist across ordinary restarts.

Use the same file list to inspect or stop the stack:

```bash
docker compose -f compose.example.yaml -f compose.dev.yaml -f compose.integration.yaml ps
docker compose -f compose.example.yaml -f compose.dev.yaml -f compose.integration.yaml down
```

Adding `-v` to `down` deletes the local Sonarr, qBittorrent, importer, and media state. The integration override is development-only: it does not change the production image or the CI container smoke test. It provides the services and shared path namespace required for a real workflow fixture, but it does not add tracker, indexer, or personal media credentials.

The complete release-path E2E is automated:

```bash
./scripts/run_integration_e2e.sh
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

Container bits are reproducible by digest, but the E2E is intentionally not hermetic: Sonarr resolves `tvdb:73871` and refreshes series metadata through its normal external metadata services. Run it with internet access; an upstream metadata outage is distinguishable from a container or importer failure in the Sonarr logs.

## Implementation layout

- `cmd/importer`: CLI and HTTP service process.
- `internal/sonarr`: Sonarr v4 API adapter.
- `internal/qbittorrent`: authenticated manifest and file-rename adapter.
- `internal/mapper`: pure deterministic `[NN].mkv` episode mapper.
- `internal/workflow`: dry-run, durable rename reconciliation, Sonarr auto/manual import, verification, and queue finalization.
- `internal/server`: health, status, dry-run, and execute HTTP endpoints.

The service uses a global advisory execute lock plus atomically replaced JSON operation records under `/data/operations`. The latest persisted result is restored by the status API after restart, and the one-shot CLI emits the full JSON result.

An operation is written as `prepared` before mutation, `rename_file_submitting` before each qBittorrent rename, `renames_verified` after the canonical manifest is proven, `manual_import_submitting` before an explicit Sonarr command, and `queue_finalizing` before each queue DELETE. qBittorrent rename is asynchronous even after HTTP 200, so file-index manifest observation is the proof of completion. If a response is lost or the process restarts, the same `downloadId` plus dry-run `planToken` resumes postcondition reconciliation without blindly repeating an uncertain mutation.

The `/data` safety contract assumes a local filesystem with advisory `flock`, atomic same-directory rename, and `fsync` semantics. Docker named volumes and local bind mounts are supported; NFS, CIFS, and object-backed mounts are not supported. Source content must be regular qBittorrent-managed torrent content; symlinked content trees are outside the supported security boundary because the Web API does not expose file type information.
