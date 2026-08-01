# Development

This document is the source of truth for the implementation stack, supported integration boundary, local checks, and container development workflow. Product motivation belongs in [`project-context.md`](project-context.md), evidence-based scope decisions belong in [`concept-review.md`](concept-review.md), and publishing rules belong in [`releases.md`](releases.md).

## Stack and support boundary

- Go 1.21 language compatibility; the container build uses the current Go 1.25 toolchain.
- Standard library only at runtime.
- Sonarr v4 through `/api/v3`.
- qBittorrent Web API v2.8.2 or newer.
- Docker Compose is the supported application startup path.

Sonarr v3 has a different manual-import wire format (`language` instead of `languages`, and no v4 release fields). Phase 0 rejects it explicitly instead of maintaining an unverified compatibility fallback.

## Checks

Run formatting and static checks locally:

```bash
gofmt -w cmd internal
go test ./...
go vet ./...
```

The preinstalled Go 1.21 linker on some recent macOS hosts needs external linking for test binaries:

```bash
go test -ldflags=-linkmode=external ./...
```

This host-specific flag is not used in the Linux container build.

Build the same validated artifact used for deployment:

```bash
docker compose -f compose.example.yaml -f compose.dev.yaml build
./scripts/verify_release_container.sh
```

Application startup must remain inside Docker Compose. Direct local commands are appropriate only for formatting, tests, static checks, and one-off scripts.

## Implementation layout

- `cmd/importer`: CLI and HTTP service process.
- `internal/sonarr`: Sonarr v4 API adapter.
- `internal/qbittorrent`: read-only qBittorrent adapter.
- `internal/mapper`: pure deterministic Phase 0 filename mapper.
- `internal/workflow`: dry-run, execution preflight, import, verification, and queue finalization.
- `internal/server`: health, status, dry-run, and execute HTTP endpoints.

Phase 0 deliberately has no SQLite database or searchable audit history. It does have the minimum durable safety state required for at-most-once mutation: a global advisory execute lock plus atomically replaced JSON operation records under `/data/operations`. The latest persisted result is restored by the status API after restart, and the one-shot CLI emits the full JSON result.

An operation is written as `prepared` before mutation, `manual_import_submitting` before the Sonarr command POST, and `queue_finalizing` before each queue DELETE. If a response is lost or the process restarts, the same `downloadId` plus dry-run `planToken` resumes postcondition reconciliation without blindly repeating an uncertain mutation. An unresolved `manual_import_submitting` state is intentionally fail-closed until Sonarr history and episode-file evidence prove success.

The `/data` safety contract assumes a local filesystem with advisory `flock`, atomic same-directory rename, and `fsync` semantics. Docker named volumes and local bind mounts are supported; NFS, CIFS, and object-backed mounts are not supported in Phase 0. SQLite audit history, broader restart recovery, polling, and multi-download scheduling remain Phase 1 work.
