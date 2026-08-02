package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zenderg/sonarr-torrent-importer/internal/qbittorrent"
	"github.com/zenderg/sonarr-torrent-importer/internal/sonarr"
)

type renameBlockedError struct {
	err error
}

func (e *renameBlockedError) Error() string {
	return e.err.Error()
}

type importPostconditionMissingError struct {
	paths []string
}

func (e *importPostconditionMissingError) Error() string {
	return fmt.Sprintf("missing verified postcondition for %s", strings.Join(e.paths, ", "))
}

type importPostconditionUnsafeError struct {
	paths []string
}

func (e *importPostconditionUnsafeError) Error() string {
	return fmt.Sprintf("episode already has a file without exact current import evidence for %s", strings.Join(e.paths, ", "))
}

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

func (e *Engine) performRenames(ctx context.Context, built *plan) error {
	for index := range built.prepared {
		prepared := &built.prepared[index]
		if prepared.renameApplied {
			continue
		}
		if prepared.originalPath == prepared.targetPath {
			if err := e.recordRenameApplied(built, index, prepared.manifest); err != nil {
				return err
			}
			continue
		}

		current, err := e.observeRenameState(ctx, built, index)
		if err != nil {
			return fmt.Errorf("reconcile qBittorrent rename for file %d: %w", prepared.manifest.Index, err)
		}
		if current.Name == prepared.targetPath {
			if err := e.recordRenameApplied(built, index, current); err != nil {
				return err
			}
			continue
		}
		if current.Name != prepared.originalPath {
			return fmt.Errorf("qBittorrent file %d has unexpected path %q while reconciling rename", current.Index, current.Name)
		}

		if err := e.saveOperation(built, "rename_file_submitting", 0); err != nil {
			return fmt.Errorf("persist qBittorrent rename intent for file %d: %w", current.Index, err)
		}
		renameErr := e.qbittorrent.RenameFile(ctx, built.torrent.Hash, prepared.originalPath, prepared.targetPath)
		observed, observeErr := e.observeRenameEventually(ctx, built, index, renameErr)
		if observeErr != nil {
			var apiError *qbittorrent.APIError
			if observed.Name == prepared.originalPath && errors.As(renameErr, &apiError) && apiError.StatusCode == 409 {
				return &renameBlockedError{err: fmt.Errorf("qBittorrent rejected rename for file %d because the source or target path is invalid or occupied: %w", current.Index, renameErr)}
			}
			return fmt.Errorf("qBittorrent rename for file %d is unresolved: %w (rename error: %v)", current.Index, observeErr, renameErr)
		}
		switch observed.Name {
		case prepared.targetPath:
			if err := e.recordRenameApplied(built, index, observed); err != nil {
				return err
			}
			event(&built.result, "source-rename", "ok", fmt.Sprintf("qBittorrent verified file %d at canonical path %q.", observed.Index, observed.Name))
		case prepared.originalPath:
			if renameErr != nil {
				return fmt.Errorf("qBittorrent did not apply rename for file %d: %w", observed.Index, renameErr)
			}
			return fmt.Errorf("qBittorrent returned success but file %d remains at its original path", observed.Index)
		default:
			return fmt.Errorf("qBittorrent file %d moved to unexpected path %q", observed.Index, observed.Name)
		}
	}
	if err := e.verifyTorrentUnchanged(ctx, built); err != nil {
		return fmt.Errorf("verify canonical qBittorrent manifest: %w", err)
	}
	if err := e.saveOperation(built, "renames_verified", 0); err != nil {
		return fmt.Errorf("persist verified qBittorrent renames: %w", err)
	}
	return nil
}

func (e *Engine) observeRenameEventually(ctx context.Context, built *plan, preparedIndex int, renameErr error) (qbittorrent.File, error) {
	timeout := e.commandTimeout
	if timeout <= 0 {
		timeout = time.Second
	}
	interval := e.pollInterval
	if interval <= 0 {
		interval = 10 * time.Millisecond
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		observed, err := e.observeRenameState(ctx, built, preparedIndex)
		if err != nil {
			return qbittorrent.File{}, err
		}
		prepared := built.prepared[preparedIndex]
		if observed.Name == prepared.targetPath {
			return observed, nil
		}
		if observed.Name != prepared.originalPath {
			return observed, fmt.Errorf("file moved to unexpected path %q", observed.Name)
		}
		var apiError *qbittorrent.APIError
		if errors.As(renameErr, &apiError) && apiError.StatusCode == 409 {
			return observed, renameErr
		}
		select {
		case <-ctx.Done():
			return observed, ctx.Err()
		case <-deadline.C:
			return observed, fmt.Errorf("timed out waiting for qBittorrent rename postcondition")
		case <-time.After(interval):
		}
	}
}

func (e *Engine) observeRenameState(ctx context.Context, built *plan, preparedIndex int) (qbittorrent.File, error) {
	torrent, err := e.qbittorrent.Torrent(ctx, built.context.DownloadID)
	if err != nil {
		return qbittorrent.File{}, fmt.Errorf("refresh torrent: %w", err)
	}
	if !strings.EqualFold(torrent.Hash, built.torrent.Hash) || torrent.Name != built.torrent.Name || torrent.SavePath != built.torrent.SavePath {
		return qbittorrent.File{}, fmt.Errorf("torrent identity, name, or storage topology changed")
	}
	if !activeSeedingState(torrent.State) {
		return qbittorrent.File{}, fmt.Errorf("torrent left active seeding state: %q", torrent.State)
	}
	files, err := e.qbittorrent.Files(ctx, torrent.Hash)
	if err != nil {
		return qbittorrent.File{}, fmt.Errorf("refresh manifest: %w", err)
	}
	if err := validateManifest(files); err != nil {
		return qbittorrent.File{}, err
	}
	if len(files) != len(built.manifest) {
		return qbittorrent.File{}, fmt.Errorf("manifest file count changed")
	}
	currentByIndex := make(map[int]qbittorrent.File, len(files))
	for _, file := range files {
		currentByIndex[file.Index] = file
	}
	prepared := built.prepared[preparedIndex]
	var observed qbittorrent.File
	observedFound := false
	for _, expected := range built.manifest {
		current, found := currentByIndex[expected.Index]
		if !found || current.Size != expected.Size || current.Progress != expected.Progress || current.Priority != expected.Priority {
			return qbittorrent.File{}, fmt.Errorf("manifest metadata changed for file %d", expected.Index)
		}
		if expected.Index == prepared.manifest.Index {
			if current.Name != prepared.originalPath && current.Name != prepared.targetPath {
				return qbittorrent.File{}, fmt.Errorf("file %d has unexpected path %q", current.Index, current.Name)
			}
			observed = current
			observedFound = true
			continue
		}
		if current.Name != expected.Name {
			return qbittorrent.File{}, fmt.Errorf("unrelated file %d path changed", current.Index)
		}
	}
	if !observedFound {
		return qbittorrent.File{}, fmt.Errorf("manifest no longer contains file %d", prepared.manifest.Index)
	}
	if err := validateTorrentContentPath(built, torrent, files); err != nil {
		return qbittorrent.File{}, err
	}
	if err := e.reconcileTorrentCategory(built, torrent, files); err != nil {
		return qbittorrent.File{}, err
	}
	return observed, nil
}

func validateTorrentContentPath(built *plan, torrent qbittorrent.Torrent, files []qbittorrent.File) error {
	initial := normalizeSonarrPath(built.torrent.ContentPath)
	current := normalizeSonarrPath(torrent.ContentPath)
	if current == initial {
		return nil
	}
	currentByIndex := make(map[int]qbittorrent.File, len(files))
	for _, file := range files {
		currentByIndex[file.Index] = file
	}
	for _, prepared := range built.prepared {
		oldRelative := normalizeRelativeForCorrelation(prepared.originalPath)
		newRelative := normalizeRelativeForCorrelation(prepared.targetPath)
		expected := ""
		switch {
		case strings.HasSuffix(initial, "/"+oldRelative):
			expected = strings.TrimSuffix(initial, oldRelative) + newRelative
		case strings.HasSuffix(initial, "/"+pathBase(oldRelative)):
			expected = strings.TrimSuffix(initial, pathBase(oldRelative)) + pathBase(newRelative)
		}
		observed, found := currentByIndex[prepared.manifest.Index]
		if expected != "" && current == expected && found && observed.Name == prepared.targetPath {
			return nil
		}
	}
	return fmt.Errorf("qBittorrent content path changed outside the exact canonical file rename")
}

func (e *Engine) reconcileTorrentCategory(built *plan, torrent qbittorrent.Torrent, files []qbittorrent.File) error {
	if torrent.Category == built.torrent.Category {
		if built.observedCategory != "" {
			return fmt.Errorf("qBittorrent category reverted after the observed Sonarr post-import transition")
		}
		return nil
	}
	if !postImportCategoryEnabled(built.queueRecords) {
		return fmt.Errorf("qBittorrent category changed although Sonarr has no post-import category policy")
	}
	if strings.TrimSpace(torrent.Category) == "" {
		return fmt.Errorf("qBittorrent category was cleared instead of changing to a configured post-import category")
	}
	canonicalObserved := false
	currentByIndex := make(map[int]qbittorrent.File, len(files))
	for _, file := range files {
		currentByIndex[file.Index] = file
	}
	for _, prepared := range built.prepared {
		if current, found := currentByIndex[prepared.manifest.Index]; found && current.Name == prepared.targetPath {
			canonicalObserved = true
			break
		}
	}
	if !canonicalObserved {
		return fmt.Errorf("qBittorrent category changed before any canonical source rename was observed")
	}
	if built.observedCategory == "" {
		built.observedCategory = torrent.Category
	} else if torrent.Category != built.observedCategory {
		return fmt.Errorf("qBittorrent post-import category changed from %q to %q", built.observedCategory, torrent.Category)
	}
	return nil
}

func postImportCategoryEnabled(records []sonarr.QueueRecord) bool {
	return len(records) > 0 && records[0].DownloadClientHasPostImportCategory
}

func (e *Engine) recordRenameApplied(built *plan, preparedIndex int, observed qbittorrent.File) error {
	prepared := &built.prepared[preparedIndex]
	prepared.renameApplied = true
	prepared.manifest = observed
	for index := range built.manifest {
		if built.manifest[index].Index == observed.Index {
			built.manifest[index] = observed
			break
		}
	}
	digest, err := manifestDigest(built.torrent, built.manifest)
	if err != nil {
		return err
	}
	built.manifestSHA256 = digest
	built.result.ManifestSHA256 = digest
	rename := built.result.Files[prepared.resultIndex].Rename
	if rename != nil && rename.Status != "not_required" {
		rename.Status = "applied"
	}
	return e.saveOperation(built, "renaming", 0)
}

func (e *Engine) prepareManualImport(ctx context.Context, built *plan) error {
	wasPrepared := manualImportPrepared(built)
	if wasPrepared {
		verifyErr := e.verifyImportedOnce(ctx, built)
		if verifyErr == nil {
			event(&built.result, "sonarr-auto-import", "ok", "Sonarr import postconditions were reconciled before resuming the prepared manual-import phase.")
			return e.saveOperation(built, "manual_import_ready", 0)
		}
		var missing *importPostconditionMissingError
		if !errors.As(verifyErr, &missing) {
			return fmt.Errorf("revalidate prepared Sonarr import state: %w", verifyErr)
		}
		if manualImportPrepared(built) {
			return nil
		}
	}

	if !wasPrepared {
		if err := e.verifyImportedEventually(ctx, built, e.commandTimeout); err == nil {
			event(&built.result, "sonarr-auto-import", "ok", "Sonarr automatically imported every file after canonical source renaming.")
			return e.saveOperation(built, "manual_import_ready", 0)
		}
	}

	candidateRoot, err := sonarrPathAfterRenames(built.outputPath, built.prepared)
	if err != nil {
		return fmt.Errorf("derive Sonarr-visible canonical path: %w", err)
	}
	candidates, err := e.sonarr.ManualImportCandidates(ctx, candidateRoot, built.context.DownloadID)
	if err != nil {
		return fmt.Errorf("discover Sonarr candidates after qBittorrent rename: %w", err)
	}
	usedCandidatePaths := make(map[string]struct{})
	for index := range built.prepared {
		prepared := &built.prepared[index]
		fileResult := &built.result.Files[prepared.resultIndex]
		if fileResult.Outcome == "imported" || fileResult.Outcome == "already_satisfied" {
			continue
		}
		fileResult.Outcome = "correlating"
		candidate, correlationErr := correlateCandidate(prepared.manifest, candidates, built.context, candidateRoot)
		if correlationErr != nil || !sameSonarrPath(candidate.Path, prepared.expectedSourcePath) {
			fileResult.Outcome = "blocked"
			fileResult.Reason = "SONARR_PATH_CORRELATION_FAILED_AFTER_RENAME"
			continue
		}
		if _, used := usedCandidatePaths[candidate.Path]; used {
			fileResult.Outcome = "blocked"
			fileResult.Reason = "SONARR_CANDIDATE_REUSED"
			continue
		}
		usedCandidatePaths[candidate.Path] = struct{}{}
		prepared.candidate = candidate
		fileResult.SonarrPath = candidate.Path
	}
	if hasBlockedFile(built.result.Files) {
		return fmt.Errorf("Sonarr did not expose one unique canonical path for every renamed media file")
	}
	if err := e.reprocess(ctx, built); err != nil {
		return err
	}
	if hasBlockedFile(built.result.Files) {
		return fmt.Errorf("Sonarr rejected or changed at least one explicit mapping after canonical rename")
	}
	event(&built.result, "sonarr-reprocess", "ok", "Sonarr accepted every canonical source path and explicit episode mapping.")
	return e.saveOperation(built, "manual_import_ready", 0)
}

func manualImportPrepared(built *plan) bool {
	for _, prepared := range built.prepared {
		outcome := built.result.Files[prepared.resultIndex].Outcome
		if prepared.commandFile.Path == "" && outcome != "imported" && outcome != "already_satisfied" {
			return false
		}
	}
	return true
}

func sonarrPathAfterRenames(originalOutputPath string, prepared []preparedFile) (string, error) {
	originalOutputPath = normalizeSonarrPath(originalOutputPath)
	if originalOutputPath == "" || len(prepared) == 0 {
		return "", fmt.Errorf("original output path or rename plan is empty")
	}
	if len(prepared) == 1 {
		oldRelative := normalizeRelativeForCorrelation(prepared[0].originalPath)
		newRelative := normalizeRelativeForCorrelation(prepared[0].targetPath)
		if strings.HasSuffix(originalOutputPath, "/"+oldRelative) {
			return strings.TrimSuffix(originalOutputPath, oldRelative) + newRelative, nil
		}
		oldBase := pathBase(oldRelative)
		if strings.HasSuffix(originalOutputPath, "/"+oldBase) {
			return strings.TrimSuffix(originalOutputPath, oldBase) + pathBase(newRelative), nil
		}
	}
	root := strings.Split(normalizeRelativeForCorrelation(prepared[0].originalPath), "/")[0]
	if root != "" && (strings.HasSuffix(originalOutputPath, "/"+root) || originalOutputPath == root) {
		for _, file := range prepared {
			if !strings.HasPrefix(normalizeRelativeForCorrelation(file.originalPath), root+"/") || !strings.HasPrefix(normalizeRelativeForCorrelation(file.targetPath), root+"/") {
				return "", fmt.Errorf("rename plan does not share one torrent root")
			}
		}
		return originalOutputPath, nil
	}
	return "", fmt.Errorf("Sonarr output path %q cannot be correlated to the original torrent paths", originalOutputPath)
}

func expectedSonarrSourcePath(originalOutputPath string, file preparedFile, prepared []preparedFile) (string, error) {
	originalOutputPath = normalizeSonarrPath(originalOutputPath)
	oldRelative := normalizeRelativeForCorrelation(file.originalPath)
	newRelative := normalizeRelativeForCorrelation(file.targetPath)
	if originalOutputPath == "" || oldRelative == "" || newRelative == "" {
		return "", fmt.Errorf("Sonarr output path or torrent rename path is empty")
	}
	if strings.HasSuffix(originalOutputPath, "/"+oldRelative) {
		return strings.TrimSuffix(originalOutputPath, oldRelative) + newRelative, nil
	}
	if len(prepared) == 1 && strings.HasSuffix(originalOutputPath, "/"+pathBase(oldRelative)) {
		return strings.TrimSuffix(originalOutputPath, pathBase(oldRelative)) + pathBase(newRelative), nil
	}
	root := strings.Split(oldRelative, "/")[0]
	if root != "" && (strings.HasSuffix(originalOutputPath, "/"+root) || originalOutputPath == root) && strings.HasPrefix(newRelative, root+"/") {
		return strings.TrimSuffix(originalOutputPath, "/") + strings.TrimPrefix(newRelative, root), nil
	}
	return "", fmt.Errorf("Sonarr output path %q cannot be bound to canonical torrent path %q", originalOutputPath, newRelative)
}

func pathBase(value string) string {
	index := strings.LastIndex(value, "/")
	if index < 0 {
		return value
	}
	return value[index+1:]
}

func (e *Engine) runPreparedOperation(ctx context.Context, built *plan) (Result, error) {
	if err := e.performRenames(ctx, built); err != nil {
		var blockedErr *renameBlockedError
		if errors.As(err, &blockedErr) {
			block(&built.result, "QBITTORRENT_RENAME_REJECTED", blockedErr.Error())
			built.result.CanExecute = false
			if saveErr := e.saveOperation(built, "rename_blocked", 0); saveErr != nil {
				return built.result, fmt.Errorf("persist blocked qBittorrent rename: %w", saveErr)
			}
			return built.result, nil
		}
		built.result.Outcome = "uncertain"
		built.result.CanExecute = false
		return built.result, err
	}
	if err := e.prepareManualImport(ctx, built); err != nil {
		built.result.Outcome = "blocked"
		built.result.CanExecute = false
		_ = e.saveOperation(built, "renames_verified", 0)
		return built.result, err
	}

	verifyErr := e.verifyImportedBeforeMutation(ctx, built)
	if verifyErr == nil {
		event(&built.result, "manual-import", "skipped", "Sonarr import postconditions were already complete immediately before command submission.")
	} else {
		var missing *importPostconditionMissingError
		if !errors.As(verifyErr, &missing) {
			built.result.Outcome = "uncertain"
			built.result.CanExecute = false
			_ = e.saveOperation(built, "manual_import_ready", 0)
			return built.result, fmt.Errorf("reconcile Sonarr import immediately before ManualImport submission: %w", verifyErr)
		}
	}

	commandFiles := make([]sonarr.ManualImportFile, 0, len(built.prepared))
	for _, prepared := range built.prepared {
		fileResult := built.result.Files[prepared.resultIndex]
		if prepared.commandFile.Path != "" && fileResult.Outcome != "imported" && fileResult.Outcome != "already_satisfied" {
			commandFiles = append(commandFiles, prepared.commandFile)
		}
	}
	if len(commandFiles) == 0 && verifyErr != nil {
		built.result.Outcome = "uncertain"
		built.result.CanExecute = false
		_ = e.saveOperation(built, "manual_import_ready", 0)
		return built.result, fmt.Errorf("no Sonarr ManualImport command is prepared for incomplete import postconditions: %w", verifyErr)
	}
	if len(commandFiles) > 0 {
		if err := e.verifyTorrentUnchanged(ctx, built); err != nil {
			built.result.Outcome = "blocked"
			built.result.CanExecute = false
			_ = e.saveOperation(built, "manual_import_ready", 0)
			return built.result, fmt.Errorf("qBittorrent preflight immediately before ManualImport submission: %w", err)
		}
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
	}
	if err := e.saveOperation(built, "import_verified", commandID(built.result.Command)); err != nil {
		return built.result, fmt.Errorf("persist verified import postconditions: %w", err)
	}
	return e.completeOperation(ctx, built)
}

func (e *Engine) completeOperation(ctx context.Context, built *plan) (Result, error) {
	if err := e.verifyImportedEventually(ctx, built, e.commandTimeout); err != nil {
		built.result.Outcome = "uncertain"
		built.result.CanExecute = false
		phase := built.result.OperationPhase
		if phase != "import_verified" && phase != "queue_finalizing" {
			phase = "import_verified"
		}
		_ = e.saveOperation(built, phase, commandID(built.result.Command))
		return built.result, fmt.Errorf("revalidate import postconditions before queue finalization: %w", err)
	}
	if err := e.saveOperation(built, "queue_finalizing", commandID(built.result.Command)); err != nil {
		return built.result, fmt.Errorf("persist queue finalization intent: %w", err)
	}
	if err := e.finalizeQueue(ctx, built); err != nil {
		built.result.Outcome = "uncertain"
		built.result.CanExecute = false
		_ = e.saveOperation(built, "queue_finalizing", commandID(built.result.Command))
		return built.result, fmt.Errorf("finalize Sonarr queue: %w", err)
	}
	if err := e.verifyImportedOnce(ctx, built); err != nil {
		built.result.Outcome = "uncertain"
		built.result.CanExecute = false
		_ = e.saveOperation(built, "queue_finalizing", commandID(built.result.Command))
		return built.result, fmt.Errorf("revalidate import postconditions before completion: %w", err)
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
	event(&built.result, "complete", "ok", "Sonarr queue is finalized and qBittorrent still owns the verified seeding torrent with canonical source filenames.")
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
	if record.Phase == "complete" {
		return built.result, nil
	}
	if err := e.qbittorrent.Login(ctx); err != nil {
		return built.result, fmt.Errorf("authenticate with qBittorrent for recovery: %w", err)
	}

	switch record.Phase {
	case "rename_blocked":
		remaining := built.result.Blockers[:0]
		for _, blocker := range built.result.Blockers {
			if blocker.Code != "QBITTORRENT_RENAME_REJECTED" {
				remaining = append(remaining, blocker)
			}
		}
		built.result.Blockers = remaining
		built.result.Outcome = "ready"
		built.result.CanExecute = true
		event(&built.result, "recovery", "retrying", "Explicit execute is retrying the persisted partial rename plan after operator remediation.")
		return e.runPreparedOperation(ctx, &built)
	case "prepared":
		if err := e.preflightUnchanged(ctx, &built); err != nil {
			block(&built.result, "STALE_EXECUTION_PLAN", err.Error())
			built.result.CanExecute = false
			return built.result, nil
		}
		return e.runPreparedOperation(ctx, &built)
	case "rename_file_submitting", "renaming", "renames_verified", "manual_import_ready":
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
	return e.verifyTorrentUnchanged(ctx, built)
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
	if timeout <= 0 {
		timeout = time.Second
	}
	interval := e.pollInterval
	if interval <= 0 {
		interval = 10 * time.Millisecond
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := e.verifyImportedOnce(ctx, built); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return e.verifyImportedOnce(ctx, built)
		case <-ticker.C:
		}
	}
}

func (e *Engine) verifyImportedBeforeMutation(ctx context.Context, built *plan) error {
	err := e.verifyImportedOnce(ctx, built)
	var unsafe *importPostconditionUnsafeError
	if errors.As(err, &unsafe) {
		return e.verifyImportedEventually(ctx, built, e.commandTimeout)
	}
	return err
}

func (e *Engine) verifyImportedOnce(ctx context.Context, built *plan) error {
	for _, prepared := range built.prepared {
		fileResult := &built.result.Files[prepared.resultIndex]
		if fileResult.Outcome == "imported" {
			fileResult.Outcome = "ready"
			fileResult.Reason = ""
			fileResult.Verification = nil
			fileResult.SonarrPath = prepared.expectedSourcePath
		}
	}
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
	unsafe := make([]string, 0)
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
			unsafe = append(unsafe, prepared.manifest.Name)
			continue
		}
		fileResult.Outcome = "imported"
		fileResult.Verification = &verification
		fileResult.SonarrPath = verification.SourcePath
	}
	if len(unsafe) > 0 {
		return &importPostconditionUnsafeError{paths: unsafe}
	}
	if len(missing) > 0 {
		return &importPostconditionMissingError{paths: missing}
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
		droppedPath := record.Data["droppedPath"]
		if prepared.expectedSourcePath == "" || !sameSonarrPath(droppedPath, prepared.expectedSourcePath) {
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
			SourcePath: droppedPath, ImportedPath: file.Path,
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
			if queueRecordIdentitySnapshot(record) != queueRecordIdentitySnapshot(original) {
				return fmt.Errorf("Sonarr queue item %d changed identity or finalization policy", record.ID)
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
				if postImportCategoryEnabled(built.queueRecords) {
					finalization.Method = "sonarr-auto-post-import-category"
				} else {
					finalization.Method = "sonarr-auto"
				}
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
		if err := e.verifyImportedOnce(ctx, built); err != nil {
			return fmt.Errorf("revalidate import postconditions before finalizing queue item %d: %w", record.ID, err)
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
	if err := validateTorrentContentPath(built, torrent, manifest); err != nil {
		return err
	}
	if err := e.reconcileTorrentCategory(built, torrent, manifest); err != nil {
		return fmt.Errorf("verify qBittorrent category after queue finalization: %w", err)
	}
	digest, err := manifestDigest(torrent, manifest)
	if err != nil {
		return err
	}
	if digest != built.manifestSHA256 {
		return fmt.Errorf("torrent manifest changed during queue finalization")
	}
	if postImportCategoryEnabled(built.queueRecords) && torrent.Category == built.torrent.Category {
		return fmt.Errorf("Sonarr has a post-import category policy but qBittorrent category did not change")
	}
	if !postImportCategoryEnabled(built.queueRecords) && torrent.Category != built.torrent.Category {
		return fmt.Errorf("qBittorrent category changed although Sonarr has no post-import category policy")
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
	if torrent.Name != built.torrent.Name || torrent.SavePath != built.torrent.SavePath {
		return fmt.Errorf("qBittorrent torrent name or storage topology changed after planning")
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
	if err := validateTorrentContentPath(built, torrent, manifest); err != nil {
		return err
	}
	if err := e.reconcileTorrentCategory(built, torrent, manifest); err != nil {
		return err
	}
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
	seriesTitle := ""
	if record.Series != nil {
		seriesTitle = strings.TrimSpace(record.Series.Title)
	}
	return fmt.Sprintf("%d:%d:%d:%s:%s:%s:%t:%s:%s:%s:%s:%s:%s",
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
		strings.ToLower(seriesTitle),
		strings.TrimSpace(string(record.Quality)),
	)
}

func queueRecordIdentitySnapshot(record sonarr.QueueRecord) string {
	seriesID, _ := queueSeriesID(record)
	seasonNumber, _ := queueSeasonNumber(record)
	return fmt.Sprintf("%d:%d:%d:%s:%s:%t:%s",
		record.ID,
		seriesID,
		seasonNumber,
		strings.ToLower(record.DownloadID),
		record.DownloadClient,
		record.DownloadClientHasPostImportCategory,
		strings.ToLower(record.Protocol),
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
