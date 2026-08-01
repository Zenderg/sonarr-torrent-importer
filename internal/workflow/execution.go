package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zenderg/sonarr-torrent-importer/internal/sonarr"
)

func (e *Engine) execute(ctx context.Context, built plan) (Result, error) {
	if err := e.preflightUnchanged(ctx, &built); err != nil {
		block(&built.result, "STALE_EXECUTION_PLAN", err.Error())
		return built.result, nil
	}
	event(&built.result, "execute-preflight", "ok", "Queue context, episode metadata, torrent state, and manifest still match the dry-run snapshot.")
	if err := e.saveOperation(&built, "prepared", 0); err != nil {
		return built.result, fmt.Errorf("persist execution intent: %w", err)
	}
	mutationContext, cancel := context.WithTimeout(context.Background(), e.workflowTimeout)
	defer cancel()
	return e.runPreparedOperation(mutationContext, &built)
}

func (e *Engine) runPreparedOperation(ctx context.Context, built *plan) (Result, error) {
	commandFiles := make([]sonarr.ManualImportFile, 0, len(built.prepared))
	for _, prepared := range built.prepared {
		commandFiles = append(commandFiles, prepared.commandFile)
	}
	if len(commandFiles) > 0 {
		if err := e.saveOperation(built, "manual_import_submitting", 0); err != nil {
			return built.result, fmt.Errorf("persist ManualImport submission intent: %w", err)
		}
		command, startErr := e.sonarr.StartManualImport(ctx, commandFiles)
		if startErr != nil {
			event(&built.result, "manual-import", "uncertain", "Sonarr command submission failed; the mutation will not be retried and postconditions will be checked.")
			if verifyErr := e.verifyImportedEventually(ctx, built, e.commandTimeout); verifyErr != nil {
				built.result.Outcome = "uncertain"
				built.result.CanExecute = false
				_ = e.saveOperation(built, "manual_import_submitting", 0)
				return built.result, fmt.Errorf("Sonarr command submission was uncertain and import postconditions were not proven: %w (submission error: %v)", verifyErr, startErr)
			}
		} else {
			built.result.Command = &CommandResult{ID: command.ID, Status: command.Status, Result: command.Result, Message: command.Message}
			if err := e.saveOperation(built, "command_accepted", command.ID); err != nil {
				return built.result, fmt.Errorf("persist accepted ManualImport command: %w", err)
			}
			event(&built.result, "manual-import", "accepted", fmt.Sprintf("Sonarr accepted ManualImport command %d.", command.ID))
			terminal, pollErr := e.pollCommand(ctx, command.ID)
			built.result.Command = &CommandResult{ID: terminal.ID, Status: terminal.Status, Result: terminal.Result, Message: terminal.Message}
			if pollErr != nil {
				event(&built.result, "manual-import", "uncertain", "Command status did not prove success; postconditions will be checked without retrying the mutation.")
				if verifyErr := e.verifyImportedOnce(ctx, built); verifyErr != nil {
					built.result.Outcome = "uncertain"
					built.result.CanExecute = false
					_ = e.saveOperation(built, "command_accepted", command.ID)
					return built.result, fmt.Errorf("ManualImport command did not prove success and postconditions are incomplete: %w (command error: %v)", verifyErr, pollErr)
				}
			} else if verifyErr := e.verifyImportedEventually(ctx, built, e.commandTimeout); verifyErr != nil {
				built.result.Outcome = "uncertain"
				built.result.CanExecute = false
				_ = e.saveOperation(built, "command_accepted", command.ID)
				return built.result, fmt.Errorf("ManualImport command completed but postconditions were not proven: %w", verifyErr)
			}
		}
		event(&built.result, "postcondition", "ok", "Every imported file is linked to new Sonarr history and the expected episode file.")
	} else {
		event(&built.result, "manual-import", "skipped", "All selected files already have a fully verified import postcondition.")
	}
	if err := e.saveOperation(built, "import_verified", commandID(built.result.Command)); err != nil {
		return built.result, fmt.Errorf("persist verified import postconditions: %w", err)
	}
	return e.completeOperation(ctx, built)
}

func (e *Engine) completeOperation(ctx context.Context, built *plan) (Result, error) {
	if err := e.saveOperation(built, "queue_finalizing", commandID(built.result.Command)); err != nil {
		return built.result, fmt.Errorf("persist queue finalization intent: %w", err)
	}
	if err := e.finalizeQueue(ctx, built); err != nil {
		built.result.Outcome = "uncertain"
		built.result.CanExecute = false
		_ = e.saveOperation(built, "queue_finalizing", commandID(built.result.Command))
		return built.result, fmt.Errorf("finalize Sonarr queue: %w", err)
	}

	imported := false
	for _, file := range built.result.Files {
		if file.Outcome == "imported" {
			imported = true
			break
		}
	}
	if imported {
		built.result.Outcome = "imported"
	} else {
		built.result.Outcome = "already_satisfied"
	}
	built.result.CanExecute = false
	event(&built.result, "complete", "ok", "Sonarr queue is finalized and qBittorrent still owns the unchanged seeding torrent.")
	if err := e.saveOperation(built, "complete", commandID(built.result.Command)); err != nil {
		return built.result, fmt.Errorf("persist completed operation: %w", err)
	}
	return built.result, nil
}

func (e *Engine) resumeOperation(ctx context.Context, record operationRecord) (Result, error) {
	built := restorePlan(record.Plan)
	built.result.Mode = "execute"
	built.result.PlanToken = record.PlanToken
	event(&built.result, "recovery", "resumed", fmt.Sprintf("Resuming durable operation from phase %s without repeating a proven or uncertain mutation.", record.Phase))

	switch record.Phase {
	case "complete":
		return built.result, nil
	case "prepared":
		if err := e.preflightUnchanged(ctx, &built); err != nil {
			block(&built.result, "STALE_EXECUTION_PLAN", err.Error())
			built.result.CanExecute = false
			return built.result, nil
		}
		return e.runPreparedOperation(ctx, &built)
	case "manual_import_submitting":
		if err := e.verifyImportedEventually(ctx, &built, e.commandTimeout); err != nil {
			built.result.Outcome = "uncertain"
			built.result.CanExecute = false
			_ = e.saveOperation(&built, record.Phase, record.CommandID)
			return built.result, fmt.Errorf("ManualImport submission remains uncertain; it will not be submitted again: %w", err)
		}
		event(&built.result, "manual-import", "reconciled", "Persisted import postconditions prove that the uncertain ManualImport submission completed.")
		if err := e.saveOperation(&built, "import_verified", record.CommandID); err != nil {
			return built.result, err
		}
		return e.completeOperation(ctx, &built)
	case "command_accepted":
		if record.CommandID <= 0 {
			return built.result, fmt.Errorf("durable command_accepted operation has no Sonarr command ID")
		}
		terminal, pollErr := e.pollCommand(ctx, record.CommandID)
		built.result.Command = &CommandResult{ID: terminal.ID, Status: terminal.Status, Result: terminal.Result, Message: terminal.Message}
		verifyErr := e.verifyImportedEventually(ctx, &built, e.commandTimeout)
		if verifyErr != nil {
			built.result.Outcome = "uncertain"
			built.result.CanExecute = false
			_ = e.saveOperation(&built, record.Phase, record.CommandID)
			return built.result, fmt.Errorf("accepted ManualImport command remains unresolved: %w (command error: %v)", verifyErr, pollErr)
		}
		event(&built.result, "manual-import", "reconciled", "The accepted ManualImport command has complete verified postconditions.")
		if err := e.saveOperation(&built, "import_verified", record.CommandID); err != nil {
			return built.result, err
		}
		return e.completeOperation(ctx, &built)
	case "import_verified", "queue_finalizing":
		return e.completeOperation(ctx, &built)
	default:
		return built.result, fmt.Errorf("unsupported durable operation phase %q", record.Phase)
	}
}

func (e *Engine) saveOperation(built *plan, phase string, commandID int) error {
	built.result.OperationPhase = phase
	record := operationRecord{
		PlanToken: built.result.PlanToken,
		Phase:     phase,
		CommandID: commandID,
		Plan:      persistPlan(*built),
	}
	return e.operations.save(built.context.DownloadID, record)
}

func commandID(command *CommandResult) int {
	if command == nil {
		return 0
	}
	return command.ID
}

func (e *Engine) preflightUnchanged(ctx context.Context, built *plan) error {
	queue, err := e.sonarr.Queue(ctx)
	if err != nil {
		return fmt.Errorf("refresh Sonarr queue: %w", err)
	}
	refreshedRecords, refreshedContext, outputPath, err := resolveQueueSelection(queue, Selection{DownloadID: built.context.DownloadID})
	if err != nil {
		return err
	}
	if queueSafetySnapshot(refreshedRecords) != queueSafetySnapshot(built.queueRecords) ||
		refreshedContext.SeriesID != built.context.SeriesID ||
		refreshedContext.SeasonNumber != built.context.SeasonNumber ||
		!strings.EqualFold(refreshedContext.DownloadID, built.context.DownloadID) ||
		!equalInts(refreshedContext.QueueIDs, built.context.QueueIDs) ||
		!sameSonarrPath(outputPath, built.outputPath) {
		return fmt.Errorf("Sonarr queue identity, path, state, or finalization policy changed after planning")
	}

	episodes, err := e.sonarr.Episodes(ctx, built.context.SeriesID, built.context.SeasonNumber)
	if err != nil {
		return fmt.Errorf("refresh Sonarr episodes: %w", err)
	}
	if episodeSnapshot(episodes) != episodeSnapshot(built.episodes) {
		return fmt.Errorf("Sonarr episode metadata changed after planning")
	}
	if len(built.prepared) > 0 {
		candidates, err := e.sonarr.ManualImportCandidates(ctx, built.outputPath, built.context.DownloadID)
		if err != nil {
			return fmt.Errorf("refresh Sonarr manual-import candidates: %w", err)
		}
		refreshed := *built
		refreshed.result.Files = append([]FileResult(nil), built.result.Files...)
		refreshed.prepared = append([]preparedFile(nil), built.prepared...)
		for index := range refreshed.prepared {
			candidate, err := correlateCandidate(refreshed.prepared[index].manifest, candidates, built.context, built.outputPath)
			if err != nil {
				return fmt.Errorf("refresh Sonarr path correlation: %w", err)
			}
			refreshed.prepared[index].candidate = candidate
			refreshed.result.Files[refreshed.prepared[index].resultIndex].Outcome = "correlating"
		}
		if err := e.reprocess(ctx, &refreshed); err != nil {
			return fmt.Errorf("refresh explicit Sonarr mappings: %w", err)
		}
		if hasBlockedFile(refreshed.result.Files) {
			return fmt.Errorf("Sonarr manual-import mapping changed or became invalid after planning")
		}
		expected, err := manualImportFilesSnapshot(built.prepared)
		if err != nil {
			return err
		}
		actual, err := manualImportFilesSnapshot(refreshed.prepared)
		if err != nil {
			return err
		}
		if actual != expected {
			return fmt.Errorf("Sonarr manual-import command payload changed after planning")
		}
	}

	return e.verifyTorrentUnchanged(ctx, built)
}

func manualImportFilesSnapshot(prepared []preparedFile) (string, error) {
	files := make([]sonarr.ManualImportFile, 0, len(prepared))
	for _, file := range prepared {
		files = append(files, file.commandFile)
	}
	encoded, err := json.Marshal(files)
	if err != nil {
		return "", fmt.Errorf("encode Sonarr manual-import command snapshot: %w", err)
	}
	return string(encoded), nil
}

func (e *Engine) pollCommand(ctx context.Context, commandID int) (sonarr.Command, error) {
	deadline := time.NewTimer(e.commandTimeout)
	defer deadline.Stop()
	interval := time.NewTicker(e.pollInterval)
	defer interval.Stop()

	last := sonarr.Command{ID: commandID, Status: "unknown"}
	for {
		command, err := e.sonarr.Command(ctx, commandID)
		if err != nil {
			return last, err
		}
		last = command
		status := strings.ToLower(command.Status)
		result := strings.ToLower(command.Result)
		if status == "completed" {
			if result == "successful" {
				return command, nil
			}
			return command, fmt.Errorf("Sonarr command completed with result %q", command.Result)
		}
		switch status {
		case "failed", "aborted", "cancelled", "orphaned":
			return command, fmt.Errorf("Sonarr command entered terminal status %q", command.Status)
		}
		if result == "unsuccessful" {
			return command, fmt.Errorf("Sonarr command reported unsuccessful result")
		}

		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-deadline.C:
			return last, fmt.Errorf("timed out waiting for Sonarr command %d", commandID)
		case <-interval.C:
		}
	}
}

func (e *Engine) classifyExisting(ctx context.Context, built *plan, history []sonarr.HistoryRecord) error {
	episodes := episodesByID(built.episodes)
	fileIDs := make([]int, 0)
	for _, prepared := range built.prepared {
		episodeID := built.result.Files[prepared.resultIndex].Mapping.EpisodeIDs[0]
		if episode, found := episodes[episodeID]; found && episode.HasFile && episode.EpisodeFileID > 0 {
			fileIDs = append(fileIDs, episode.EpisodeFileID)
		}
	}
	files, err := e.loadEpisodeFiles(ctx, fileIDs)
	if err != nil {
		return err
	}
	for _, prepared := range built.prepared {
		fileResult := &built.result.Files[prepared.resultIndex]
		episodeID := fileResult.Mapping.EpisodeIDs[0]
		episode, found := episodes[episodeID]
		if !found {
			fileResult.Outcome = "blocked"
			fileResult.Reason = "EPISODE_METADATA_DISAPPEARED"
			continue
		}
		if !episode.HasFile {
			continue
		}
		verification, verified := verifyEvidence(prepared, episode, files, history, nil)
		if !verified {
			fileResult.Outcome = "blocked"
			fileResult.Reason = "EPISODE_ALREADY_HAS_UNVERIFIED_FILE"
			continue
		}
		fileResult.Outcome = "already_satisfied"
		fileResult.Verification = &verification
	}
	return nil
}

func (e *Engine) verifyImportedEventually(ctx context.Context, built *plan, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	interval := time.NewTicker(e.pollInterval)
	defer interval.Stop()
	for {
		if err := e.verifyImportedOnce(ctx, built); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return e.verifyImportedOnce(ctx, built)
		case <-interval.C:
		}
	}
}

func (e *Engine) verifyImportedOnce(ctx context.Context, built *plan) error {
	history, err := e.sonarr.History(ctx, built.context.DownloadID)
	if err != nil {
		return fmt.Errorf("refresh Sonarr history: %w", err)
	}
	episodes, err := e.sonarr.Episodes(ctx, built.context.SeriesID, built.context.SeasonNumber)
	if err != nil {
		return fmt.Errorf("refresh Sonarr episodes: %w", err)
	}
	episodesByID := episodesByID(episodes)
	fileIDs := make([]int, 0, len(built.prepared))
	for _, prepared := range built.prepared {
		episodeID := built.result.Files[prepared.resultIndex].Mapping.EpisodeIDs[0]
		if episode, found := episodesByID[episodeID]; found && episode.HasFile && episode.EpisodeFileID > 0 {
			fileIDs = append(fileIDs, episode.EpisodeFileID)
		}
	}
	files, err := e.loadEpisodeFiles(ctx, fileIDs)
	if err != nil {
		return err
	}
	missing := make([]string, 0)
	for _, prepared := range built.prepared {
		fileResult := &built.result.Files[prepared.resultIndex]
		episodeID := fileResult.Mapping.EpisodeIDs[0]
		episode, found := episodesByID[episodeID]
		if !found || !episode.HasFile {
			missing = append(missing, prepared.manifest.Name)
			continue
		}
		verification, verified := verifyEvidence(prepared, episode, files, history, built.historyBaseline)
		if !verified {
			missing = append(missing, prepared.manifest.Name)
			continue
		}
		fileResult.Outcome = "imported"
		fileResult.Verification = &verification
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing verified postcondition for %s", strings.Join(missing, ", "))
	}
	return nil
}

func verifyEvidence(prepared preparedFile, episode sonarr.Episode, files map[int]sonarr.EpisodeFile, history []sonarr.HistoryRecord, baseline map[int]struct{}) (Verification, bool) {
	file, found := files[episode.EpisodeFileID]
	if !found || file.ID != episode.EpisodeFileID || file.SeriesID != episode.SeriesID || file.SeasonNumber != episode.SeasonNumber || file.Size != prepared.manifest.Size {
		return Verification{}, false
	}
	for _, record := range history {
		if baseline != nil {
			if _, existed := baseline[record.ID]; existed {
				continue
			}
		}
		if record.EpisodeID != episode.ID || record.SeriesID != episode.SeriesID || !strings.EqualFold(record.DownloadID, prepared.candidate.DownloadID) || !strings.EqualFold(record.EventType, "downloadFolderImported") {
			continue
		}
		if !sameSonarrPath(record.Data["droppedPath"], prepared.candidate.Path) {
			continue
		}
		fileID, err := strconv.Atoi(record.Data["fileId"])
		if err != nil || fileID != episode.EpisodeFileID {
			continue
		}
		importedPath := record.Data["importedPath"]
		if !sameSonarrPath(importedPath, file.Path) {
			continue
		}
		return Verification{
			HistoryID: record.ID, EpisodeID: episode.ID, EpisodeFileID: file.ID,
			SourcePath: prepared.candidate.Path, ImportedPath: file.Path,
		}, true
	}
	return Verification{}, false
}

func (e *Engine) loadEpisodeFiles(ctx context.Context, ids []int) (map[int]sonarr.EpisodeFile, error) {
	unique := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		if id > 0 {
			unique[id] = struct{}{}
		}
	}
	if len(unique) == 0 {
		return map[int]sonarr.EpisodeFile{}, nil
	}
	ordered := make([]int, 0, len(unique))
	for id := range unique {
		ordered = append(ordered, id)
	}
	sort.Ints(ordered)
	files, err := e.sonarr.EpisodeFiles(ctx, ordered)
	if err != nil {
		return nil, fmt.Errorf("read Sonarr episode files: %w", err)
	}
	result := make(map[int]sonarr.EpisodeFile, len(files))
	for _, file := range files {
		result[file.ID] = file
	}
	return result, nil
}

func (e *Engine) finalizeQueue(ctx context.Context, built *plan) error {
	finalization := built.result.QueueFinalization
	if finalization == nil || finalization.Status == "verified" {
		finalization = &QueueFinalization{
			Status: "pending", CategoryBefore: built.torrent.Category,
		}
		built.result.QueueFinalization = finalization
	}
	originalByID := make(map[int]sonarr.QueueRecord, len(built.queueRecords))
	for _, record := range built.queueRecords {
		originalByID[record.ID] = record
	}
	usedChangeCategory := false
	for _, id := range finalization.FinalizedQueueIDs {
		if originalByID[id].DownloadClientHasPostImportCategory {
			usedChangeCategory = true
		}
	}
	for attempts := 0; attempts < len(built.queueRecords)+1; attempts++ {
		queue, err := e.sonarr.Queue(ctx)
		if err != nil {
			return fmt.Errorf("refresh queue before finalization: %w", err)
		}
		matched := queueByDownloadID(queue, built.context.DownloadID)
		for _, record := range matched {
			original, tracked := originalByID[record.ID]
			if !tracked {
				return fmt.Errorf("Sonarr exposed a new queue item %d for the download during finalization", record.ID)
			}
			if queueRecordSafetySnapshot(record) != queueRecordSafetySnapshot(original) {
				return fmt.Errorf("Sonarr queue item %d changed identity, path, state, or finalization policy", record.ID)
			}
		}
		if len(matched) == 0 {
			if finalization.PendingQueueID > 0 {
				if _, tracked := originalByID[finalization.PendingQueueID]; !tracked {
					return fmt.Errorf("pending queue item %d is absent from the durable plan", finalization.PendingQueueID)
				}
				if !containsInt(finalization.FinalizedQueueIDs, finalization.PendingQueueID) {
					finalization.FinalizedQueueIDs = append(finalization.FinalizedQueueIDs, finalization.PendingQueueID)
				}
				usedChangeCategory = usedChangeCategory || finalization.PendingChangeCategory
				finalization.PendingQueueID = 0
				finalization.PendingChangeCategory = false
				if err := e.saveOperation(built, "queue_finalizing", commandID(built.result.Command)); err != nil {
					return fmt.Errorf("persist reconciled queue finalization: %w", err)
				}
			}
			if len(finalization.FinalizedQueueIDs) == 0 {
				finalization.Method = "sonarr-auto"
			} else if usedChangeCategory {
				finalization.Method = "sonarr-post-import-category"
			} else {
				finalization.Method = "sonarr-ignore"
			}
			break
		}
		sort.Slice(matched, func(i, j int) bool { return matched[i].ID < matched[j].ID })
		record := matched[0]
		if finalization.PendingQueueID > 0 {
			found := false
			for _, candidate := range matched {
				if candidate.ID == finalization.PendingQueueID {
					record = candidate
					found = true
					break
				}
			}
			if !found {
				if !containsInt(finalization.FinalizedQueueIDs, finalization.PendingQueueID) {
					finalization.FinalizedQueueIDs = append(finalization.FinalizedQueueIDs, finalization.PendingQueueID)
				}
				usedChangeCategory = usedChangeCategory || finalization.PendingChangeCategory
				finalization.PendingQueueID = 0
				finalization.PendingChangeCategory = false
				if err := e.saveOperation(built, "queue_finalizing", commandID(built.result.Command)); err != nil {
					return fmt.Errorf("persist reconciled queue item: %w", err)
				}
				continue
			}
			if err := e.verifyTorrentUnchanged(ctx, built); err != nil {
				return fmt.Errorf("reconcile pending queue item %d: %w", finalization.PendingQueueID, err)
			}
			return fmt.Errorf("queue item %d is still present after an uncertain prior DELETE; the mutation will not be repeated automatically", finalization.PendingQueueID)
		}
		if err := e.verifyTorrentUnchanged(ctx, built); err != nil {
			return fmt.Errorf("preflight queue item %d finalization: %w", record.ID, err)
		}
		changeCategory := finalization.PendingChangeCategory
		if finalization.PendingQueueID == 0 {
			changeCategory = record.DownloadClientHasPostImportCategory && !usedChangeCategory
			finalization.PendingQueueID = record.ID
			finalization.PendingChangeCategory = changeCategory
			if err := e.saveOperation(built, "queue_finalizing", commandID(built.result.Command)); err != nil {
				return fmt.Errorf("persist queue item %d finalization intent: %w", record.ID, err)
			}
		}
		finalizeErr := e.sonarr.FinalizeQueue(ctx, record.ID, changeCategory)
		if finalizeErr != nil {
			refreshed, refreshErr := e.sonarr.Queue(ctx)
			if refreshErr != nil {
				return fmt.Errorf("finalize queue item %d had an uncertain result and queue reconciliation failed: %w (delete error: %v)", record.ID, refreshErr, finalizeErr)
			}
			if queueContainsID(refreshed, record.ID) {
				return fmt.Errorf("finalize queue item %d failed and the item is still present: %w", record.ID, finalizeErr)
			}
			if err := e.verifyTorrentUnchanged(ctx, built); err != nil {
				return fmt.Errorf("finalize queue item %d had an uncertain result and torrent postconditions failed: %w (delete error: %v)", record.ID, err, finalizeErr)
			}
			event(&built.result, "queue-finalization", "reconciled", fmt.Sprintf("Queue item %d disappeared despite an uncertain DELETE response; torrent postconditions remained valid.", record.ID))
		}
		usedChangeCategory = usedChangeCategory || changeCategory
		if !containsInt(finalization.FinalizedQueueIDs, record.ID) {
			finalization.FinalizedQueueIDs = append(finalization.FinalizedQueueIDs, record.ID)
		}
		finalization.PendingQueueID = 0
		finalization.PendingChangeCategory = false
		if err := e.saveOperation(built, "queue_finalizing", commandID(built.result.Command)); err != nil {
			return fmt.Errorf("persist finalized queue item %d: %w", record.ID, err)
		}
	}
	queue, err := e.sonarr.Queue(ctx)
	if err != nil {
		return fmt.Errorf("verify queue finalization: %w", err)
	}
	if len(queueByDownloadID(queue, built.context.DownloadID)) != 0 {
		return fmt.Errorf("Sonarr still exposes queue items for the processed download")
	}

	torrent, err := e.qbittorrent.Torrent(ctx, built.context.DownloadID)
	if err != nil {
		return fmt.Errorf("verify torrent still exists: %w", err)
	}
	if !activeSeedingState(torrent.State) {
		return fmt.Errorf("torrent no longer has an active seeding state after finalization: %q", torrent.State)
	}
	manifest, err := e.qbittorrent.Files(ctx, torrent.Hash)
	if err != nil {
		return fmt.Errorf("verify torrent manifest after finalization: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return err
	}
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Index < manifest[j].Index })
	digest, err := manifestDigest(torrent, manifest)
	if err != nil {
		return err
	}
	if digest != built.manifestSHA256 {
		return fmt.Errorf("torrent manifest changed during queue finalization")
	}
	if usedChangeCategory && torrent.Category == built.torrent.Category {
		return fmt.Errorf("Sonarr reported queue finalization but qBittorrent category did not change")
	}
	if !usedChangeCategory && len(finalization.FinalizedQueueIDs) > 0 && torrent.Category != built.torrent.Category {
		return fmt.Errorf("qBittorrent category changed unexpectedly during ignore finalization")
	}
	finalization.Status = "verified"
	finalization.CategoryAfter = torrent.Category
	return nil
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (e *Engine) verifyTorrentUnchanged(ctx context.Context, built *plan) error {
	torrent, err := e.qbittorrent.Torrent(ctx, built.context.DownloadID)
	if err != nil {
		return fmt.Errorf("refresh qBittorrent torrent: %w", err)
	}
	if !strings.EqualFold(torrent.Hash, built.torrent.Hash) {
		return fmt.Errorf("qBittorrent returned a different torrent identity")
	}
	if !activeSeedingState(torrent.State) {
		return fmt.Errorf("qBittorrent torrent left active seeding state: %q", torrent.State)
	}
	manifest, err := e.qbittorrent.Files(ctx, torrent.Hash)
	if err != nil {
		return fmt.Errorf("refresh qBittorrent manifest: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return err
	}
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Index < manifest[j].Index })
	digest, err := manifestDigest(torrent, manifest)
	if err != nil {
		return err
	}
	if digest != built.manifestSHA256 {
		return fmt.Errorf("qBittorrent manifest changed after planning")
	}
	return nil
}

func queueByDownloadID(queue []sonarr.QueueRecord, downloadID string) []sonarr.QueueRecord {
	matched := make([]sonarr.QueueRecord, 0)
	for _, record := range queue {
		if strings.EqualFold(record.DownloadID, downloadID) {
			matched = append(matched, record)
		}
	}
	return matched
}

func queueContainsID(queue []sonarr.QueueRecord, id int) bool {
	for _, record := range queue {
		if record.ID == id {
			return true
		}
	}
	return false
}

func queueSafetySnapshot(records []sonarr.QueueRecord) string {
	values := make([]string, 0, len(records))
	for _, record := range records {
		values = append(values, queueRecordSafetySnapshot(record))
	}
	sort.Strings(values)
	return strings.Join(values, "|")
}

func queueRecordSafetySnapshot(record sonarr.QueueRecord) string {
	seriesID, _ := queueSeriesID(record)
	seasonNumber, _ := queueSeasonNumber(record)
	return fmt.Sprintf("%d:%d:%d:%s:%s:%s:%t:%s:%s:%s:%s",
		record.ID,
		seriesID,
		seasonNumber,
		strings.ToLower(record.DownloadID),
		normalizeSonarrPath(record.OutputPath),
		record.DownloadClient,
		record.DownloadClientHasPostImportCategory,
		strings.ToLower(record.Protocol),
		strings.ToLower(record.Status),
		strings.ToLower(record.TrackedDownloadState),
		strings.ToLower(record.TrackedDownloadStatus),
	)
}

func episodesByID(episodes []sonarr.Episode) map[int]sonarr.Episode {
	result := make(map[int]sonarr.Episode, len(episodes))
	for _, episode := range episodes {
		result[episode.ID] = episode
	}
	return result
}

func episodeSnapshot(episodes []sonarr.Episode) string {
	type snapshot struct {
		ID, SeriesID, SeasonNumber, EpisodeNumber, EpisodeFileID int
		HasFile                                                  bool
	}
	values := make([]snapshot, 0, len(episodes))
	for _, episode := range episodes {
		values = append(values, snapshot{
			ID: episode.ID, SeriesID: episode.SeriesID, SeasonNumber: episode.SeasonNumber,
			EpisodeNumber: episode.EpisodeNumber, EpisodeFileID: episode.EpisodeFileID,
			HasFile: episode.HasFile,
		})
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	var builder strings.Builder
	for _, value := range values {
		fmt.Fprintf(&builder, "%d:%d:%d:%d:%d:%t;", value.ID, value.SeriesID, value.SeasonNumber, value.EpisodeNumber, value.EpisodeFileID, value.HasFile)
	}
	return builder.String()
}

func sameSonarrPath(left, right string) bool {
	return normalizeSonarrPath(left) == normalizeSonarrPath(right)
}
