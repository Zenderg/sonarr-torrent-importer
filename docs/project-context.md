# Project Context

This document records the original product problem, goals, and proposed design for `sonarr-torrent-importer`. It is the source of truth for product motivation and design hypotheses, not for validated implementation behavior. The evidence-based MVP boundaries and corrections belong in [`concept-review.md`](concept-review.md), while packaging and release rules belong in [`releases.md`](releases.md).

## Summary

`sonarr-torrent-importer` is a companion service for Sonarr and qBittorrent. It is intended to make completed-download importing reliable for releases that do not fit Sonarr's usual assumption that a torrent is an immutable, fully identifiable release.

The project focuses on two common classes of difficult downloads:

- files whose names do not contain enough information for Sonarr to identify the season and episode automatically;
- rolling or mutable season torrents whose contents grow as new episodes are released.

Anime releases expose these problems frequently, but the design must be generic and work for ordinary TV series as well.

## Motivation

Sonarr works best when every downloaded file has an unambiguous name such as `Show.Name.S02E03.mkv` and when a grabbed torrent never changes after it is published. Some trackers and release groups use different conventions:

- episode numbers may be relative to a season, for example `[01].mkv`, while the surrounding torrent name is the only place that identifies the season;
- a season pack may initially contain only the episodes that have aired;
- the tracker may later replace the torrent metadata with a new revision containing additional episodes;
- the release title or tracker topic may remain stable even though the torrent info hash and file list change;
- an indexer may describe the release as a complete season, causing Sonarr to associate the download with episodes that are not present yet.

These cases create three related user-facing problems:

1. Completed files require repetitive manual mapping and importing.
2. Sonarr Activity may retain misleading queue entries for an entire season even after all files that actually exist have finished downloading.
3. When a rolling torrent gains a new episode, rechecking the old torrent cannot discover the new file because the old torrent metadata does not contain it.

Manual import is useful for diagnosis and exceptional ambiguity, but it is not an acceptable recurring workflow.

## Core Principle

The service should treat the torrent's actual file list as the source of truth for what was downloaded, and use Sonarr metadata as the source of truth for the series and episode identities.

It should bridge the gap between those two models without replacing Sonarr, qBittorrent, or the indexer stack.

## Goals

- Import completed episode files automatically without weekly manual intervention.
- Support both standard TV and anime naming schemes.
- Infer missing season context from the tracked download, torrent name, Sonarr queue, and Sonarr episode metadata.
- Preserve seeding by renaming files through qBittorrent rather than moving files behind the client's back.
- Ensure Sonarr only imports episodes backed by real, completed video files.
- Remove successfully handled downloads from Sonarr's active queue view without deleting the torrent data.
- Detect new revisions of rolling torrents and reuse existing data when downloading newly added episodes.
- Make every workflow idempotent, observable, and safe to retry.
- Provide a dry-run mode that explains proposed mappings and mutations.
- Run as a small self-hosted service, with Docker as a first-class deployment option.

## Non-Goals

- Replacing Sonarr's library management, monitoring, or metadata provider.
- Replacing qBittorrent as the download and seeding client.
- Replacing Prowlarr or other indexers as the general-purpose search layer.
- Transcoding, repairing, or otherwise modifying media contents.
- Guessing through genuinely ambiguous mappings without evidence.
- Deleting downloaded data as part of the initial implementation.
- Encoding assumptions about one tracker, one release group, or anime-only numbering into the core domain model.

## Terminology

- **Tracked download**: a torrent known to Sonarr or explicitly enrolled in this service.
- **Rolling torrent**: a logical release whose torrent metadata is periodically replaced to add or change files.
- **Torrent revision**: one immutable torrent metadata document, identified by its info hash and file manifest.
- **Release identity**: a stable logical identity used to associate multiple torrent revisions, such as tracker, topic or GUID, normalized title, series, and season.
- **Canonical filename**: an unambiguous name containing enough information for Sonarr, normally including `SxxEyy` or an equivalent supported date/absolute-number form.
- **Managed category**: a qBittorrent category observed by the service.
- **Post-import category**: a category used for successfully imported torrents that should continue seeding but no longer appear as active Sonarr downloads.

## Target Workflow

### 1. Discover a completed download

The service observes explicitly configured qBittorrent categories or tags. It correlates a torrent with Sonarr queue/history data and resolves the intended series and, when possible, season.

Only completed video files selected for download are considered. Future episodes inferred by an indexer but absent from the torrent manifest are ignored.

### 2. Map files to episodes

Mapping should use an ordered evidence pipeline:

1. Parse explicit standard patterns such as `S02E03`, `2x03`, multi-episode ranges, or air dates.
2. Parse an explicit absolute episode number when Sonarr metadata supports it.
3. Parse a local episode number such as `[03]` and combine it with season context from the torrent-level release and Sonarr.
4. Consult persisted release-group or release-specific mapping rules.
5. Compare candidates with the Sonarr episode list, air dates, existing episode files, and the set of files in the torrent.
6. Stop for manual review when more than one plausible mapping remains.

The mapper must produce an explanation and confidence level for every decision. A mapping should never be accepted merely because it is the only numerically possible result.

### 3. Normalize paths without breaking seeding

When a file name is not parseable by Sonarr, the service should use qBittorrent's file-rename API to assign a canonical path. qBittorrent must remain aware of the renamed path so that seeding continues normally.

Renames must be collision-safe, reversible in state, and applied only after validating every target path in the batch.

### 4. Import and verify

The service asks Sonarr to import the mapped files, preferably with explicit episode identities rather than relying on a second round of filename guessing.

An import is successful only after verification against Sonarr history and episode-file state. The service must not treat an accepted API request as proof of completion.

### 5. Finalize the queue state

After all real, completed files in the current torrent revision are imported and verified, the torrent can be moved to a configured post-import qBittorrent category.

This keeps the torrent available for seeding while preventing an indexer's season-pack interpretation from leaving misleading entries in Sonarr Activity. Missing episodes remain monitored in Sonarr; only the handled download leaves the active queue.

### 6. Handle a rolling-torrent revision

For an enrolled rolling release, the service periodically checks an indexer/source adapter for updated torrent metadata.

When a new revision is found:

1. Fetch and validate the new torrent metadata.
2. Compare its info hash and normalized file manifest with the stored revision.
3. Pause the old torrent before any potentially conflicting operation.
4. Add the new revision in a paused state using the same intended data location.
5. Apply the same canonical qBittorrent path mappings.
6. Recheck the new revision so existing pieces are reused.
7. Resume it to download only missing or changed data.
8. Run the normal mapping and import workflow for newly completed files.
9. Remove the old torrent record only after the replacement is healthy and all required imports are verified.

Removing an obsolete torrent record must not delete its data. Destructive cleanup, if ever added, must be a separate opt-in policy with additional safeguards.

## Proposed Components

### Sonarr adapter

- Read series, seasons, episodes, queue, history, and episode-file state.
- Resolve the Sonarr identity associated with a download.
- Submit explicit import operations or trigger an appropriate downloaded-files scan.
- Verify import results.

### qBittorrent adapter

- Read torrents, categories, tags, state, save paths, and file manifests.
- Rename individual files while preserving torrent ownership of the path.
- Pause, resume, recheck, add, categorize, and remove torrent records.
- Never request file deletion in the initial implementation.

### Source adapters

- Discover new torrent revisions through Prowlarr or a tracker-specific integration.
- Expose a common revision model to the core workflow.
- Keep tracker-specific authentication and parsing outside the mapping engine.

The core should not require a source adapter for basic completed-download importing. Rolling-torrent updates are an additional capability.

### Mapping engine

- Parse filenames and release names.
- Combine file-level and torrent-level context.
- Query Sonarr metadata through a narrow interface.
- Produce deterministic mappings with explanations and confidence.
- Support persisted overrides without hard-coding a release group into the application.

### Workflow engine

- Model operations as resumable state transitions.
- Use idempotency keys for imports, renames, and torrent revisions.
- Verify postconditions after every external mutation.
- Recover safely after a process restart or API timeout.

### State store

SQLite is sufficient for the initial scope. Expected records include:

- managed release identities;
- observed torrent revisions and manifests;
- file-to-episode mappings and their evidence;
- applied qBittorrent renames;
- import attempts and verification results;
- user-approved overrides;
- workflow events and errors.

## Safety Invariants

- Never delete media data by default.
- Never remove an old torrent record until its replacement is validated.
- Never mark a workflow complete until Sonarr import state is verified.
- Never import an ambiguous file automatically.
- Never log API keys, passwords, cookies, full authorization headers, or torrent credentials.
- Never embed deployment-specific addresses or paths in source code or documentation examples.
- Never assume that a qBittorrent `100%` status proves that a file is valid media.
- Treat media analysis failures as a blocked import that requires recheck, redownload, or operator review.
- Make repeated polling and retries safe; the same file must not produce duplicate imports or repeated renames.

## Configuration and Privacy

All endpoints, credentials, categories, tags, paths, polling intervals, and source-specific settings must be supplied at runtime, preferably through environment variables or mounted configuration.

The public repository should contain a sanitized `.env.example` with placeholders only. Logs, fixtures, screenshots, issue templates, and test data must not expose real infrastructure, credentials, private tracker data, or personal information.

Example media and release names used in tests should be fictional or clearly generic.

## Observability

Every managed torrent should have a readable timeline containing:

- why it was selected;
- which series and season were resolved;
- the actual qBittorrent file manifest;
- proposed and accepted episode mappings;
- path renames;
- Sonarr import requests and verification results;
- category changes;
- detected rolling revisions;
- recheck and replacement outcomes;
- blocked decisions and the evidence required to unblock them.

Structured logs and a minimal status API are required for the MVP. A small web UI may be added later, but correctness and auditability take priority.

## MVP Scope

The first milestone should solve recurring manual imports before attempting tracker-specific rolling updates.

### Phase 1: Reliable completed-download import

- Connect to Sonarr and qBittorrent.
- Observe configured categories/tags.
- Correlate a torrent with a Sonarr series and season.
- Read the real completed file manifest.
- Map common standard, date, absolute, and season-relative patterns.
- Show a dry-run explanation.
- Rename files through qBittorrent when required.
- Import with explicit episode mappings.
- Verify the result in Sonarr.
- Move verified torrents to a post-import category.
- Persist workflow state and audit events.

### Phase 2: Rules and manual review

- Persist narrowly scoped mapping overrides.
- Provide a review API or small UI for ambiguous cases.
- Reapply approved rules automatically to future revisions.
- Add media-file validation hooks before import.

### Phase 3: Rolling-torrent revisions

- Introduce the source-adapter interface.
- Track logical releases separately from torrent revisions.
- Detect manifest changes.
- Safely replace torrents while reusing existing data.
- Import only newly added or changed episodes.
- Retire obsolete torrent records without deleting data.

## Testing Strategy

- Unit-test filename and release parsing with table-driven fictional fixtures.
- Unit-test ambiguous cases and confidence thresholds.
- Contract-test Sonarr and qBittorrent adapters against recorded, sanitized responses.
- Integration-test idempotency, restart recovery, partial API failures, and duplicate events.
- Test that qBittorrent renames remain seedable.
- Test rolling revisions where files are added, replaced, removed, or reordered.
- Test that future Sonarr episodes absent from the torrent manifest do not become import work.
- Test that no deletion request can be emitted by the default configuration.

## Success Criteria

- A completed torrent with season-relative names can be imported without per-file weekly intervention when sufficient torrent-level context exists.
- Standard TV, date-based shows, and absolute-numbered anime use the same core pipeline.
- Every imported file is associated with the intended Sonarr episode and can be explained by the audit log.
- Successfully handled torrents leave Sonarr Activity while continuing to seed.
- Reprocessing the same torrent or restarting the service produces no duplicate imports.
- A new rolling-torrent revision reuses valid existing data and imports only newly available episodes.
- An ambiguous or invalid file stops safely without deleting data or corrupting library state.

## Open Design Decisions

- Implementation language and framework.
- The most reliable Sonarr import API workflow for explicit episode mappings.
- How downloads are enrolled: category, tag, Sonarr queue presence, or an explicit allowlist.
- The stable release-identity strategy across different indexers and trackers.
- Confidence thresholds and the format of persisted mapping overrides.
- Whether the MVP needs a web UI or only an API and structured logs.
- How source adapters obtain updated torrent metadata without coupling credentials to the core service.
- How long obsolete torrent revisions should remain available after a verified replacement.

These decisions should be resolved with small integration prototypes before committing to a large application structure.
