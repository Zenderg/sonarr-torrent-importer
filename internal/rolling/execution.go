package rolling

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zenderg/sonarr-torrent-importer/internal/qbittorrent"
	"github.com/zenderg/sonarr-torrent-importer/internal/sonarr"
)

func (e *Engine) advance(ctx context.Context, release *Release) error {
	if release.Operation == nil || release.CandidateRevision == nil {
		return fmt.Errorf("rolling release has no complete active operation")
	}
	if err := e.qbittorrent.Login(ctx); err != nil {
		return fmt.Errorf("authenticate with qBittorrent: %w", err)
	}
	for step := 0; step < 32; step++ {
		progressed, waiting, err := e.advanceOne(ctx, release)
		if err != nil {
			if ctx.Err() != nil {
				release.Status = "updating"
				release.BlockedReason = ""
				return ctx.Err()
			}
			if release.Status == "blocked" {
				_ = e.saveRelease(release)
			}
			return err
		}
		if progressed {
			if err := e.saveRelease(release); err != nil {
				return err
			}
		}
		if waiting || !progressed || release.Operation == nil {
			return nil
		}
	}
	return fmt.Errorf("rolling reconciler exceeded its synchronous transition limit")
}

func (e *Engine) advanceOne(ctx context.Context, release *Release) (bool, bool, error) {
	operation := release.Operation
	candidate := release.CandidateRevision
	if err := e.verifyRevisionPath(*candidate); err != nil {
		return false, false, blocked(release, err.Error())
	}
	if err := e.verifyRevisionPath(release.CurrentRevision); err != nil {
		return false, false, blocked(release, err.Error())
	}
	switch operation.Phase {
	case "prepared":
		if err := e.ensureStagingSpace(*candidate); err != nil {
			return false, false, blocked(release, err.Error())
		}
		operation.Phase = "copying"
		return true, false, nil

	case "copying":
		for index := range candidate.Files {
			if candidate.Files[index].EpisodeID == 0 || candidate.Files[index].Copied {
				continue
			}
			if err := e.copyOwnedFile(ctx, release, index); err != nil {
				return false, false, blocked(release, fmt.Sprintf("safe reuse copy failed: %v", err))
			}
			return true, false, nil
		}
		operation.Phase = "copied"
		return true, false, nil

	case "copied":
		if _, exists, err := e.torrentIfExists(ctx, candidate.TorrentID); err != nil {
			return false, false, err
		} else if exists {
			return false, false, blocked(release, "candidate torrent existed in qBittorrent before its durable add intent")
		}
		operation.Phase = "new_add_submitting"
		if err := e.saveRelease(release); err != nil {
			return false, false, err
		}
		raw, err := e.store.loadArtifact(candidate.ArtifactSHA256)
		if err != nil {
			return false, false, err
		}
		if err := e.qbittorrent.AddTorrent(ctx, raw, qbittorrent.AddTorrentOptions{SavePath: candidate.SavePath, Category: candidate.Category, Tags: candidate.Tags}); err != nil {
			var rejected *qbittorrent.AddTorrentRejectedError
			if errors.As(err, &rejected) {
				operation.Phase = "copied"
				return true, false, blocked(release, fmt.Sprintf("qBittorrent definitely rejected candidate add; remediate qBittorrent and explicitly retry: %v", rejected))
			}
			return false, true, fmt.Errorf("submit candidate torrent: %w", err)
		}
		return false, true, nil

	case "new_add_submitting":
		torrent, exists, err := e.torrentIfExists(ctx, candidate.TorrentID)
		if err != nil {
			return false, false, err
		}
		if !exists {
			if time.Since(operation.UpdatedAt) >= e.commandTimeout {
				return false, false, blocked(release, "uncertain qBittorrent add has no observed torrent by its durable deadline and will not be repeated")
			}
			return false, true, nil
		}
		if candidate.AddedOn == 0 {
			candidate.AddedOn = torrent.AddedOn
		}
		if err := e.verifyCandidateTorrent(ctx, torrent, candidate, false); err != nil {
			return false, false, blocked(release, err.Error())
		}
		if checkingState(torrent.State) {
			return false, true, nil
		}
		if !stoppedState(torrent.State) {
			return false, false, blocked(release, fmt.Sprintf("candidate was not added stopped; observed state %q", torrent.State))
		}
		operation.Phase = "new_added_stopped"
		return true, false, nil

	case "new_added_stopped":
		operation.Phase = "new_recheck_submitting"
		if err := e.saveRelease(release); err != nil {
			return false, false, err
		}
		if err := e.qbittorrent.Recheck(ctx, candidate.TorrentID); err != nil {
			return false, true, fmt.Errorf("submit candidate recheck: %w", err)
		}
		operation.Phase = "new_rechecking"
		return true, true, nil

	case "new_recheck_submitting":
		torrent, exists, err := e.torrentIfExists(ctx, candidate.TorrentID)
		if err != nil {
			return false, false, err
		}
		if !exists {
			return false, false, blocked(release, "candidate disappeared while reconciling its initial recheck")
		}
		if checkingState(torrent.State) {
			operation.Phase = "new_rechecking"
			return true, true, nil
		}
		if !stoppedState(torrent.State) {
			return false, false, blocked(release, fmt.Sprintf("candidate entered unexpected state %q before recheck", torrent.State))
		}
		if err := e.qbittorrent.Recheck(ctx, candidate.TorrentID); err != nil {
			return false, true, err
		}
		operation.Phase = "new_rechecking"
		return true, true, nil

	case "new_rechecking":
		torrent, exists, err := e.torrentIfExists(ctx, candidate.TorrentID)
		if err != nil {
			return false, false, err
		}
		if !exists {
			return false, false, blocked(release, "candidate disappeared during its initial recheck")
		}
		if checkingState(torrent.State) {
			return false, true, nil
		}
		if !stoppedState(torrent.State) {
			return false, false, blocked(release, fmt.Sprintf("candidate recheck ended in unexpected state %q", torrent.State))
		}
		if err := e.verifyCandidateTorrent(ctx, torrent, candidate, false); err != nil {
			return false, false, blocked(release, err.Error())
		}
		properties, err := e.qbittorrent.Properties(ctx, candidate.TorrentID)
		if err != nil {
			return false, false, err
		}
		candidate.ReusedBytes = int64(properties.PiecesHave) * candidate.PieceLength
		if candidate.ReusedBytes > candidate.TotalLength {
			candidate.ReusedBytes = candidate.TotalLength
		}
		operation.Phase = "new_force_start_submitting"
		if err := e.saveRelease(release); err != nil {
			return false, false, err
		}
		if err := e.qbittorrent.SetForceStart(ctx, candidate.TorrentID, true); err != nil {
			return false, true, fmt.Errorf("force-start candidate download: %w", err)
		}
		return false, true, nil

	case "new_force_start_submitting":
		torrent, exists, err := e.torrentIfExists(ctx, candidate.TorrentID)
		if err != nil {
			return false, false, err
		}
		if !exists {
			return false, false, blocked(release, "candidate disappeared while force-starting its download")
		}
		if err := e.verifyCandidateTorrent(ctx, torrent, candidate, false); err != nil {
			return false, false, blocked(release, err.Error())
		}
		if errorState(torrent.State) {
			return false, false, blocked(release, fmt.Sprintf("candidate entered qBittorrent error state %q while force-starting", torrent.State))
		}
		if !torrent.ForceStart {
			if e.commandPhaseExpired(operation) {
				return false, false, blocked(release, "qBittorrent did not apply candidate force-start by its durable deadline")
			}
			if err := e.qbittorrent.SetForceStart(ctx, candidate.TorrentID, true); err != nil {
				return false, true, err
			}
			return false, true, nil
		}
		if stoppedState(torrent.State) {
			if e.commandPhaseExpired(operation) {
				return false, false, blocked(release, "force-started candidate remained stopped past its durable deadline")
			}
			if err := e.qbittorrent.Start(ctx, candidate.TorrentID); err != nil {
				return false, true, err
			}
			return false, true, nil
		}
		if checkingState(torrent.State) || movingState(torrent.State) || torrent.State == "queuedDL" || torrent.State == "queuedUP" {
			if e.commandPhaseExpired(operation) {
				return false, false, blocked(release, fmt.Sprintf("force-started candidate remained in qBittorrent state %q past its durable deadline", torrent.State))
			}
			return false, true, nil
		}
		operation.Phase = "new_downloading"
		return true, false, nil

	case "new_downloading":
		torrent, exists, err := e.torrentIfExists(ctx, candidate.TorrentID)
		if err != nil {
			return false, false, err
		}
		if !exists {
			return false, false, blocked(release, "candidate disappeared while downloading")
		}
		if !torrent.ForceStart {
			if err := e.qbittorrent.SetForceStart(ctx, candidate.TorrentID, true); err != nil {
				return false, true, err
			}
			return false, true, nil
		}
		if errorState(torrent.State) || stoppedState(torrent.State) {
			return false, false, blocked(release, fmt.Sprintf("candidate cannot finish from qBittorrent state %q", torrent.State))
		}
		if torrent.State == "queuedDL" || torrent.State == "queuedUP" {
			if e.commandPhaseExpired(operation) {
				return false, false, blocked(release, fmt.Sprintf("force-started candidate remained queued in qBittorrent state %q", torrent.State))
			}
			return false, true, nil
		}
		if checkingState(torrent.State) || movingState(torrent.State) || torrent.Progress < 1 {
			return false, true, nil
		}
		if err := e.verifyCandidateTorrent(ctx, torrent, candidate, false); err != nil {
			return false, false, blocked(release, err.Error())
		}
		for index := range candidate.Files {
			file := &candidate.Files[index]
			if file.EpisodeID == 0 || file.ImportNeeded {
				continue
			}
			digest, err := e.digestRemoteFile(ctx, path.Join(candidate.SavePath, file.RawPath), file.Size)
			if err != nil {
				return false, false, blocked(release, fmt.Sprintf("verify reused episode %d: %v", file.EpisodeID, err))
			}
			if digest != file.ContentSHA256 {
				return false, false, blocked(release, fmt.Sprintf("candidate changes content of already imported episode %d", file.EpisodeID))
			}
		}
		operation.Phase = "new_stop_submitting"
		if err := e.saveRelease(release); err != nil {
			return false, false, err
		}
		if err := e.qbittorrent.Stop(ctx, candidate.TorrentID); err != nil {
			return false, true, err
		}
		return false, true, nil

	case "new_stop_submitting":
		torrent, exists, err := e.torrentIfExists(ctx, candidate.TorrentID)
		if err != nil {
			return false, false, err
		}
		if !exists {
			return false, false, blocked(release, "candidate disappeared while stopping for canonicalization")
		}
		if !stoppedState(torrent.State) {
			if checkingState(torrent.State) || movingState(torrent.State) {
				return false, true, nil
			}
			if err := e.qbittorrent.Stop(ctx, candidate.TorrentID); err != nil {
				return false, true, err
			}
			return false, true, nil
		}
		operation.Phase = "new_stopped"
		return true, false, nil

	case "new_stopped", "renaming":
		torrent, exists, err := e.torrentIfExists(ctx, candidate.TorrentID)
		if err != nil {
			return false, false, err
		}
		if !exists {
			return false, false, blocked(release, "candidate disappeared during canonicalization")
		}
		if !stoppedState(torrent.State) {
			return false, false, blocked(release, fmt.Sprintf("candidate left stopped state during canonicalization: %q", torrent.State))
		}
		files, err := e.qbittorrent.Files(ctx, candidate.TorrentID)
		if err != nil {
			return false, false, err
		}
		byIndex := qbitFilesByIndex(files)
		for index := range candidate.Files {
			planned := &candidate.Files[index]
			if planned.CanonicalPath == "" || planned.RenameApplied {
				continue
			}
			observed, ok := byIndex[planned.Index]
			if !ok || observed.Size != planned.Size {
				return false, false, blocked(release, fmt.Sprintf("candidate manifest drifted at file %d", planned.Index))
			}
			if observed.Name == planned.CanonicalPath {
				planned.CurrentPath = observed.Name
				planned.RenameApplied = true
				return true, false, nil
			}
			if observed.Name != planned.RawPath {
				return false, false, blocked(release, fmt.Sprintf("candidate file %d has unexpected path %q", planned.Index, observed.Name))
			}
			operation.Phase = "rename_file_submitting"
			operation.MutationFileIndex = planned.Index
			if err := e.saveRelease(release); err != nil {
				return false, false, err
			}
			if err := e.qbittorrent.RenameFile(ctx, candidate.TorrentID, planned.RawPath, planned.CanonicalPath); err != nil {
				return false, true, fmt.Errorf("canonicalize candidate file %d: %w", planned.Index, err)
			}
			return false, true, nil
		}
		operation.Phase = "canonicalized"
		return true, false, nil

	case "rename_file_submitting":
		planned := revisionFileByIndex(candidate, operation.MutationFileIndex)
		if planned == nil || planned.CanonicalPath == "" {
			return false, false, blocked(release, "durable rename intent references an invalid candidate file")
		}
		files, err := e.qbittorrent.Files(ctx, candidate.TorrentID)
		if err != nil {
			return false, false, err
		}
		observed, ok := qbitFilesByIndex(files)[planned.Index]
		if !ok {
			return false, false, blocked(release, "candidate manifest lost the rename target file")
		}
		if observed.Name == planned.CanonicalPath {
			planned.CurrentPath = observed.Name
			planned.RenameApplied = true
			operation.MutationFileIndex = 0
			operation.Phase = "renaming"
			return true, false, nil
		}
		if observed.Name != planned.RawPath {
			return false, false, blocked(release, fmt.Sprintf("candidate rename resolved to unexpected path %q", observed.Name))
		}
		if err := e.qbittorrent.RenameFile(ctx, candidate.TorrentID, planned.RawPath, planned.CanonicalPath); err != nil {
			return false, true, err
		}
		return false, true, nil

	case "canonicalized":
		operation.Phase = "canonical_force_start_submitting"
		if err := e.saveRelease(release); err != nil {
			return false, false, err
		}
		if err := e.qbittorrent.SetForceStart(ctx, candidate.TorrentID, true); err != nil {
			return false, true, err
		}
		return false, true, nil

	case "canonical_force_start_submitting":
		torrent, exists, err := e.torrentIfExists(ctx, candidate.TorrentID)
		if err != nil {
			return false, false, err
		}
		if !exists {
			return false, false, blocked(release, "candidate disappeared after canonicalization")
		}
		if err := e.verifyCandidateTorrent(ctx, torrent, candidate, true); err != nil {
			return false, false, blocked(release, err.Error())
		}
		if errorState(torrent.State) {
			return false, false, blocked(release, fmt.Sprintf("candidate entered qBittorrent error state %q after canonicalization", torrent.State))
		}
		if !torrent.ForceStart {
			if e.commandPhaseExpired(operation) {
				return false, false, blocked(release, "qBittorrent did not restore candidate force-start after canonicalization")
			}
			if err := e.qbittorrent.SetForceStart(ctx, candidate.TorrentID, true); err != nil {
				return false, true, err
			}
			return false, true, nil
		}
		if stoppedState(torrent.State) {
			if e.commandPhaseExpired(operation) {
				return false, false, blocked(release, "force-started candidate remained stopped after canonicalization")
			}
			if err := e.qbittorrent.Start(ctx, candidate.TorrentID); err != nil {
				return false, true, err
			}
			return false, true, nil
		}
		if !activeSeedingState(torrent.State) || torrent.Progress != 1 {
			if checkingState(torrent.State) || movingState(torrent.State) || torrent.State == "queuedUP" {
				if e.commandPhaseExpired(operation) {
					return false, false, blocked(release, fmt.Sprintf("force-started candidate remained in qBittorrent state %q after canonicalization", torrent.State))
				}
				return false, true, nil
			}
			return false, false, blocked(release, fmt.Sprintf("candidate entered unexpected state %q after canonicalization", torrent.State))
		}
		operation.Phase = "import_preparing"
		return true, false, nil

	case "import_preparing":
		files, err := e.prepareImportFiles(ctx, *release)
		if err != nil {
			return false, false, blocked(release, err.Error())
		}
		operation.ImportFiles = files
		operation.Phase = "manual_import_ready"
		return true, false, nil

	case "manual_import_ready":
		complete, err := e.verifyImports(ctx, release)
		if err != nil {
			return false, false, err
		}
		if complete {
			operation.Phase = "imports_verified"
			return true, false, nil
		}
		if len(operation.ImportFiles) == 0 {
			return false, false, blocked(release, "no explicit Sonarr import files were prepared")
		}
		operation.Phase = "manual_import_submitting"
		operation.UpdatedAt = time.Now().UTC()
		if err := e.saveRelease(release); err != nil {
			return false, false, err
		}
		command, err := e.sonarr.StartManualImportWithMode(ctx, operation.ImportFiles, "copy")
		if err != nil {
			return false, true, fmt.Errorf("Sonarr ManualImport submission is uncertain: %w", err)
		}
		operation.CommandID = command.ID
		operation.CommandAcceptedAt = time.Now().UTC()
		operation.Phase = "command_accepted"
		return true, true, nil

	case "manual_import_submitting":
		complete, err := e.verifyImports(ctx, release)
		if err != nil {
			return false, false, err
		}
		if complete {
			operation.Phase = "imports_verified"
			return true, false, nil
		}
		if time.Since(operation.UpdatedAt) >= e.commandTimeout {
			return false, false, blocked(release, "uncertain Sonarr ManualImport submission has no verified postcondition and will not be repeated")
		}
		return false, true, nil

	case "command_accepted":
		if operation.CommandID <= 0 {
			return false, false, blocked(release, "accepted Sonarr command has no durable command id")
		}
		complete, err := e.verifyImports(ctx, release)
		if err != nil {
			return false, true, err
		}
		if complete {
			operation.Phase = "imports_verified"
			return true, false, nil
		}
		acceptedAt := operation.CommandAcceptedAt
		if acceptedAt.IsZero() {
			acceptedAt = operation.UpdatedAt
		}
		if time.Since(acceptedAt) >= e.commandTimeout {
			return false, false, blocked(release, "Sonarr ManualImport exceeded its durable deadline without complete import receipts; it will not be repeated")
		}
		command, err := e.sonarr.Command(ctx, operation.CommandID)
		if err != nil {
			return false, true, err
		}
		status := strings.ToLower(command.Status)
		if status != "completed" {
			switch status {
			case "failed", "aborted", "cancelled", "orphaned":
				return false, false, blocked(release, fmt.Sprintf("Sonarr command entered terminal status %q", command.Status))
			default:
				return false, true, nil
			}
		}
		if !strings.EqualFold(command.Result, "successful") {
			return false, false, blocked(release, fmt.Sprintf("Sonarr command completed with result %q", command.Result))
		}
		complete, err = e.verifyImports(ctx, release)
		if err != nil {
			return false, false, err
		}
		if !complete {
			return false, true, nil
		}
		operation.Phase = "imports_verified"
		return true, false, nil

	case "imports_verified":
		operation.Phase = "post_import_stop_submitting"
		if err := e.saveRelease(release); err != nil {
			return false, false, err
		}
		if err := e.qbittorrent.Stop(ctx, candidate.TorrentID); err != nil {
			return false, true, err
		}
		return false, true, nil

	case "post_import_stop_submitting":
		torrent, exists, err := e.torrentIfExists(ctx, candidate.TorrentID)
		if err != nil {
			return false, false, err
		}
		if !exists {
			return false, false, blocked(release, "candidate disappeared after Sonarr import")
		}
		if errorState(torrent.State) {
			return false, false, blocked(release, fmt.Sprintf("candidate entered qBittorrent error state %q after Sonarr import", torrent.State))
		}
		if checkingState(torrent.State) || movingState(torrent.State) {
			return false, true, nil
		}
		if !stoppedState(torrent.State) {
			if err := e.qbittorrent.Stop(ctx, candidate.TorrentID); err != nil {
				return false, true, err
			}
			return false, true, nil
		}
		operation.Phase = "post_import_stopped"
		return true, false, nil

	case "post_import_stopped":
		operation.Phase = "post_import_recheck_submitting"
		if err := e.saveRelease(release); err != nil {
			return false, false, err
		}
		if err := e.qbittorrent.Recheck(ctx, candidate.TorrentID); err != nil {
			return false, true, err
		}
		operation.Phase = "post_import_rechecking"
		return true, true, nil

	case "post_import_recheck_submitting":
		torrent, exists, err := e.torrentIfExists(ctx, candidate.TorrentID)
		if err != nil {
			return false, false, err
		}
		if !exists {
			return false, false, blocked(release, "candidate disappeared while reconciling its post-import recheck")
		}
		if errorState(torrent.State) {
			return false, false, blocked(release, fmt.Sprintf("candidate entered qBittorrent error state %q before its post-import recheck", torrent.State))
		}
		if checkingState(torrent.State) {
			operation.Phase = "post_import_rechecking"
			return true, true, nil
		}
		if !stoppedState(torrent.State) {
			return false, false, blocked(release, fmt.Sprintf("candidate entered unexpected state %q before its post-import recheck", torrent.State))
		}
		if err := e.qbittorrent.Recheck(ctx, candidate.TorrentID); err != nil {
			return false, true, err
		}
		operation.Phase = "post_import_rechecking"
		return true, true, nil

	case "post_import_rechecking":
		torrent, exists, err := e.torrentIfExists(ctx, candidate.TorrentID)
		if err != nil {
			return false, false, err
		}
		if !exists {
			return false, false, blocked(release, "candidate disappeared during its post-import recheck")
		}
		if checkingState(torrent.State) {
			return false, true, nil
		}
		if errorState(torrent.State) || !stoppedState(torrent.State) {
			return false, false, blocked(release, fmt.Sprintf("candidate post-import recheck ended in unsafe state %q", torrent.State))
		}
		if err := e.verifyCandidateTorrent(ctx, torrent, candidate, true); err != nil {
			return false, false, blocked(release, err.Error())
		}
		properties, err := e.qbittorrent.Properties(ctx, candidate.TorrentID)
		if err != nil {
			return false, false, err
		}
		if properties.PiecesNum <= 0 || properties.PiecesHave != properties.PiecesNum {
			return false, false, blocked(release, "candidate lost torrent pieces during Sonarr import")
		}
		if err := e.verifyCandidateContent(ctx, candidate, true); err != nil {
			return false, false, blocked(release, err.Error())
		}
		operation.Phase = "post_import_force_start_submitting"
		if err := e.saveRelease(release); err != nil {
			return false, false, err
		}
		if err := e.qbittorrent.SetForceStart(ctx, candidate.TorrentID, true); err != nil {
			return false, true, err
		}
		return false, true, nil

	case "post_import_force_start_submitting":
		torrent, exists, err := e.torrentIfExists(ctx, candidate.TorrentID)
		if err != nil {
			return false, false, err
		}
		if !exists {
			return false, false, blocked(release, "candidate disappeared while restarting after its post-import recheck")
		}
		if errorState(torrent.State) {
			return false, false, blocked(release, fmt.Sprintf("candidate entered qBittorrent error state %q while restarting after import", torrent.State))
		}
		if err := e.verifyCandidateTorrent(ctx, torrent, candidate, true); err != nil {
			return false, false, blocked(release, err.Error())
		}
		if !torrent.ForceStart {
			if e.commandPhaseExpired(operation) {
				return false, false, blocked(release, "qBittorrent did not restore candidate force-start after import")
			}
			if err := e.qbittorrent.SetForceStart(ctx, candidate.TorrentID, true); err != nil {
				return false, true, err
			}
			return false, true, nil
		}
		if stoppedState(torrent.State) {
			if e.commandPhaseExpired(operation) {
				return false, false, blocked(release, "force-started candidate remained stopped after import")
			}
			if err := e.qbittorrent.Start(ctx, candidate.TorrentID); err != nil {
				return false, true, err
			}
			return false, true, nil
		}
		if checkingState(torrent.State) || movingState(torrent.State) || torrent.State == "queuedUP" {
			if e.commandPhaseExpired(operation) {
				return false, false, blocked(release, fmt.Sprintf("force-started candidate remained in qBittorrent state %q after import", torrent.State))
			}
			return false, true, nil
		}
		if !activeSeedingState(torrent.State) || torrent.Progress != 1 {
			return false, false, blocked(release, fmt.Sprintf("candidate entered unexpected state %q while restarting after import", torrent.State))
		}
		operation.Phase = "retirement_ready"
		return true, false, nil

	case "retirement_ready":
		if err := e.verifyRetirementGate(ctx, *release); err != nil {
			return false, false, blocked(release, err.Error())
		}
		operation.Phase = "old_stop_submitting"
		if err := e.saveRelease(release); err != nil {
			return false, false, err
		}
		if err := e.qbittorrent.Stop(ctx, operation.OldTorrentID); err != nil {
			return false, true, err
		}
		return false, true, nil

	case "old_stop_submitting":
		old, exists, err := e.torrentIfExists(ctx, operation.OldTorrentID)
		if err != nil {
			return false, false, err
		}
		if !exists {
			return false, false, blocked(release, "old torrent disappeared before its keep-content retirement intent")
		}
		if err := e.verifyOldTorrent(ctx, old, release.CurrentRevision); err != nil {
			return false, false, blocked(release, err.Error())
		}
		if !stoppedState(old.State) {
			if err := e.qbittorrent.Stop(ctx, operation.OldTorrentID); err != nil {
				return false, true, err
			}
			return false, true, nil
		}
		operation.Phase = "old_stopped"
		return true, false, nil

	case "old_stopped":
		if err := e.verifyRetirementGate(ctx, *release); err != nil {
			return false, false, blocked(release, err.Error())
		}
		old, exists, err := e.torrentIfExists(ctx, operation.OldTorrentID)
		if err != nil || !exists {
			return false, false, blocked(release, fmt.Sprintf("old torrent is absent before retirement intent: %v", err))
		}
		if !stoppedState(old.State) {
			return false, false, blocked(release, fmt.Sprintf("old torrent is not stopped before retirement: %q", old.State))
		}
		if err := e.verifyOldTorrent(ctx, old, release.CurrentRevision); err != nil {
			return false, false, blocked(release, err.Error())
		}
		if err := e.verifyRetainedOldFiles(ctx, release.CurrentRevision); err != nil {
			return false, false, blocked(release, err.Error())
		}
		operation.Phase = "old_delete_submitting"
		if err := e.saveRelease(release); err != nil {
			return false, false, err
		}
		if err := e.qbittorrent.DeleteTorrentRecord(ctx, operation.OldTorrentID); err != nil {
			return false, true, err
		}
		return false, true, nil

	case "old_delete_submitting":
		old, exists, err := e.torrentIfExists(ctx, operation.OldTorrentID)
		if err != nil {
			return false, false, err
		}
		if exists {
			if !stoppedState(old.State) {
				return false, false, blocked(release, "old torrent changed state during retirement")
			}
			if err := e.verifyOldTorrent(ctx, old, release.CurrentRevision); err != nil {
				return false, false, blocked(release, err.Error())
			}
			if err := e.verifyRetirementGate(ctx, *release); err != nil {
				return false, false, blocked(release, err.Error())
			}
			if err := e.qbittorrent.DeleteTorrentRecord(ctx, operation.OldTorrentID); err != nil {
				return false, true, err
			}
			return false, true, nil
		}
		if err := e.verifyRetainedOldFiles(ctx, release.CurrentRevision); err != nil {
			return false, false, blocked(release, err.Error())
		}
		operation.Phase = "old_absent_verified"
		return true, false, nil

	case "old_absent_verified":
		if err := e.verifyRetirementGate(ctx, *release); err != nil {
			return false, false, blocked(release, err.Error())
		}
		if err := e.verifyRetainedOldFiles(ctx, release.CurrentRevision); err != nil {
			return false, false, blocked(release, err.Error())
		}
		for index := range candidate.Files {
			file := &candidate.Files[index]
			if file.EpisodeID == 0 {
				continue
			}
			digest, err := e.digestRemoteFile(ctx, path.Join(candidate.SavePath, file.CurrentPath), file.Size)
			if err != nil {
				return false, false, blocked(release, fmt.Sprintf("hash completed canonical source %q: %v", file.CurrentPath, err))
			}
			file.ContentSHA256 = digest
			file.Copied = false
			file.RenameApplied = false
			file.ImportNeeded = false
		}
		release.CurrentRevision = *candidate
		release.CandidateRevision = nil
		release.Operation = nil
		release.Status = "current"
		release.BlockedReason = ""
		release.LastCheckedAt = time.Now().UTC()
		release.NextCheckAt = release.LastCheckedAt.Add(e.revisionInterval)
		return true, false, nil

	default:
		return false, false, blocked(release, fmt.Sprintf("unsupported rolling operation phase %q", operation.Phase))
	}
}

func (e *Engine) verifyRevisionPath(revision Revision) error {
	expected, err := e.sonarrPath(revision.SavePath)
	if err != nil {
		return err
	}
	if path.Clean(revision.SonarrSavePath) != expected {
		return fmt.Errorf("durable Sonarr/qBittorrent storage mapping drifted")
	}
	return nil
}

func (e *Engine) verifyOldTorrent(ctx context.Context, torrent qbittorrent.Torrent, revision Revision) error {
	if !revisionMatchesTorrent(revision, torrent) || torrent.Name != revision.Name || path.Clean(torrent.SavePath) != path.Clean(revision.SavePath) {
		return fmt.Errorf("old qBittorrent torrent identity or storage topology drifted")
	}
	if revision.AddedOn != 0 && torrent.AddedOn != revision.AddedOn {
		return fmt.Errorf("old qBittorrent torrent instance fingerprint changed")
	}
	files, err := e.qbittorrent.Files(ctx, revision.TorrentID)
	if err != nil {
		return err
	}
	if len(files) != len(revision.Files) {
		return fmt.Errorf("old qBittorrent manifest count changed")
	}
	byIndex := qbitFilesByIndex(files)
	for _, planned := range revision.Files {
		observed, ok := byIndex[planned.Index]
		if !ok || observed.Name != planned.CurrentPath || observed.Size != planned.Size || observed.Progress != 1 || observed.Priority == 0 {
			return fmt.Errorf("old qBittorrent file %d drifted before retirement", planned.Index)
		}
	}
	return nil
}

func (e *Engine) saveRelease(release *Release) error {
	release.Version = recordVersion
	release.UpdatedAt = time.Now().UTC()
	if release.Operation != nil {
		release.Operation.UpdatedAt = release.UpdatedAt
	}
	return e.store.save(*release)
}

func (e *Engine) verifyCandidateTorrent(ctx context.Context, torrent qbittorrent.Torrent, candidate *Revision, canonical bool) error {
	if !revisionMatchesTorrent(*candidate, torrent) {
		return fmt.Errorf("qBittorrent candidate identity drifted")
	}
	if path.Clean(torrent.SavePath) != path.Clean(candidate.SavePath) || torrent.Name != candidate.Name {
		return fmt.Errorf("qBittorrent candidate storage topology drifted")
	}
	if candidate.AddedOn != 0 && torrent.AddedOn != candidate.AddedOn {
		return fmt.Errorf("qBittorrent candidate instance fingerprint changed")
	}
	if torrent.Category != candidate.Category {
		return fmt.Errorf("qBittorrent candidate category drifted from %q to %q", candidate.Category, torrent.Category)
	}
	files, err := e.qbittorrent.Files(ctx, candidate.TorrentID)
	if err != nil {
		return err
	}
	if len(files) != len(candidate.Files) {
		return fmt.Errorf("qBittorrent candidate manifest count changed")
	}
	byIndex := qbitFilesByIndex(files)
	for _, planned := range candidate.Files {
		observed, ok := byIndex[planned.Index]
		expectedPath := planned.RawPath
		if canonical && planned.CanonicalPath != "" {
			expectedPath = planned.CanonicalPath
		}
		if !ok || observed.Name != expectedPath || observed.Size != planned.Size || observed.Priority == 0 {
			return fmt.Errorf("qBittorrent candidate file %d no longer matches the immutable manifest", planned.Index)
		}
		if torrent.Progress == 1 && observed.Progress != 1 {
			return fmt.Errorf("completed candidate has incomplete file %d", planned.Index)
		}
	}
	return nil
}

func (e *Engine) prepareImportFiles(ctx context.Context, release Release) ([]sonarr.ManualImportFile, error) {
	candidate := release.CandidateRevision
	if candidate == nil {
		return nil, errors.New("candidate revision is missing")
	}
	episodes, err := e.sonarr.Episodes(ctx, release.SeriesID, release.SeasonNumber)
	if err != nil {
		return nil, fmt.Errorf("refresh Sonarr episodes: %w", err)
	}
	byID := make(map[int]sonarr.Episode, len(episodes))
	for _, episode := range episodes {
		byID[episode.ID] = episode
	}
	for _, file := range candidate.Files {
		if file.EpisodeID == 0 || file.ImportNeeded {
			continue
		}
		episode := byID[file.EpisodeID]
		if !episode.HasFile || episode.EpisodeFile == nil || episode.EpisodeFileID != file.EpisodeFileID || episode.EpisodeFile.Size != file.Size || !samePath(episode.EpisodeFile.Path, file.LibraryPath) {
			return nil, fmt.Errorf("existing episode %d ownership changed before import", file.EpisodeID)
		}
	}
	root := path.Join(candidate.SonarrSavePath, candidate.Name)
	// A torrent added by the rolling engine is intentionally not a Sonarr
	// tracked download. Supplying its unknown hash makes Sonarr return an empty
	// candidate set, so discovery is folder-bound; the exact hash is attached to
	// the explicit reprocess/import command and verified in history afterward.
	candidates, err := e.sonarr.ManualImportCandidates(ctx, root, "")
	if err != nil {
		return nil, fmt.Errorf("discover Sonarr manual-import candidates for directly managed torrent: %w", err)
	}
	byPath := make(map[string][]sonarr.ManualImportCandidate)
	for _, item := range candidates {
		key := normalizePath(item.Path)
		byPath[key] = append(byPath[key], item)
	}
	reprocess := make([]sonarr.ManualImportReprocess, 0)
	seasonNumber := release.SeasonNumber
	for _, file := range candidate.Files {
		if !file.ImportNeeded || file.Imported {
			continue
		}
		expected := path.Join(candidate.SonarrSavePath, file.CurrentPath)
		matches := byPath[normalizePath(expected)]
		if len(matches) != 1 || matches[0].Size != file.Size {
			return nil, fmt.Errorf("Sonarr exposed %d exact candidates for %q", len(matches), expected)
		}
		item := matches[0]
		reprocess = append(reprocess, sonarr.ManualImportReprocess{
			Path: item.Path, SeriesID: release.SeriesID, SeasonNumber: &seasonNumber, EpisodeIDs: []int{file.EpisodeID},
			Quality: item.Quality, Languages: item.Languages, ReleaseGroup: item.ReleaseGroup,
			DownloadID: candidate.TorrentID, IndexerFlags: item.IndexerFlags, ReleaseType: item.ReleaseType,
		})
	}
	if len(reprocess) == 0 {
		return nil, nil
	}
	reprocessed, err := e.sonarr.Reprocess(ctx, reprocess)
	if err != nil {
		return nil, fmt.Errorf("reprocess explicit Sonarr mappings: %w", err)
	}
	if len(reprocessed) != len(reprocess) {
		return nil, fmt.Errorf("Sonarr reprocessed %d of %d explicit candidates", len(reprocessed), len(reprocess))
	}
	responseByPath := make(map[string]sonarr.ManualImportReprocess, len(reprocessed))
	for _, item := range reprocessed {
		if _, duplicate := responseByPath[normalizePath(item.Path)]; duplicate {
			return nil, fmt.Errorf("Sonarr returned duplicate reprocess path %q", item.Path)
		}
		responseByPath[normalizePath(item.Path)] = item
	}
	files := make([]sonarr.ManualImportFile, 0, len(reprocess))
	for _, request := range reprocess {
		item, found := responseByPath[normalizePath(request.Path)]
		responseEpisodeIDs := make([]int, 0, len(item.Episodes))
		for _, episode := range item.Episodes {
			if episode.SeriesID != release.SeriesID || episode.SeasonNumber != release.SeasonNumber {
				return nil, fmt.Errorf("Sonarr changed series/season context for %q", request.Path)
			}
			responseEpisodeIDs = append(responseEpisodeIDs, episode.ID)
		}
		if !found || len(item.Rejections) != 0 || item.SeriesID != release.SeriesID || item.SeasonNumber == nil || *item.SeasonNumber != release.SeasonNumber || !strings.EqualFold(item.DownloadID, candidate.TorrentID) || len(responseEpisodeIDs) != 1 || responseEpisodeIDs[0] != request.EpisodeIDs[0] {
			return nil, fmt.Errorf("Sonarr rejected or changed explicit mapping for %q", request.Path)
		}
		if !rawPresent(item.Quality) || !rawPresent(item.Languages) || !rawPresent(item.ReleaseType) {
			return nil, fmt.Errorf("Sonarr omitted required import attributes for %q", request.Path)
		}
		files = append(files, sonarr.ManualImportFile{
			Path: item.Path, FolderName: "", SeriesID: release.SeriesID, EpisodeIDs: request.EpisodeIDs,
			Quality: item.Quality, Languages: item.Languages, ReleaseGroup: item.ReleaseGroup,
			IndexerFlags: item.IndexerFlags, ReleaseType: item.ReleaseType, DownloadID: candidate.TorrentID,
		})
	}
	return files, nil
}

func rawPresent(raw []byte) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func (e *Engine) verifyImports(ctx context.Context, release *Release) (bool, error) {
	candidate := release.CandidateRevision
	episodes, err := e.sonarr.Episodes(ctx, release.SeriesID, release.SeasonNumber)
	if err != nil {
		return false, err
	}
	byID := make(map[int]sonarr.Episode, len(episodes))
	fileIDs := make([]int, 0)
	for _, episode := range episodes {
		byID[episode.ID] = episode
		if episode.HasFile && episode.EpisodeFileID > 0 {
			fileIDs = append(fileIDs, episode.EpisodeFileID)
		}
	}
	episodeFiles, err := e.sonarr.EpisodeFiles(ctx, fileIDs)
	if err != nil {
		return false, err
	}
	filesByID := make(map[int]sonarr.EpisodeFile, len(episodeFiles))
	for _, file := range episodeFiles {
		filesByID[file.ID] = file
	}
	history, err := e.sonarr.ImportHistorySince(ctx, release.Operation.StartedAt.Add(-time.Second))
	if err != nil {
		return false, err
	}
	baseline := make(map[int]struct{}, len(release.Operation.HistoryBaseline))
	for _, id := range release.Operation.HistoryBaseline {
		baseline[id] = struct{}{}
	}
	complete := true
	for index := range candidate.Files {
		planned := &candidate.Files[index]
		if planned.EpisodeID == 0 {
			continue
		}
		episode, exists := byID[planned.EpisodeID]
		if !planned.ImportNeeded {
			if !exists || !episode.HasFile || episode.EpisodeFileID != planned.EpisodeFileID {
				return false, fmt.Errorf("existing episode %d ownership changed", planned.EpisodeID)
			}
			continue
		}
		if !exists || !episode.HasFile || episode.EpisodeFileID <= 0 {
			complete = false
			continue
		}
		file, ok := filesByID[episode.EpisodeFileID]
		if !ok || file.SeriesID != release.SeriesID || file.SeasonNumber != release.SeasonNumber || file.Size != planned.Size {
			return false, fmt.Errorf("episode file evidence for episode %d is unsafe", planned.EpisodeID)
		}
		expectedSource := path.Join(candidate.SonarrSavePath, planned.CurrentPath)
		verified := false
		for _, record := range history {
			if _, existed := baseline[record.ID]; existed {
				continue
			}
			if record.EpisodeID != planned.EpisodeID || record.SeriesID != release.SeriesID || (record.DownloadID != "" && !strings.EqualFold(record.DownloadID, candidate.TorrentID)) || !strings.EqualFold(record.EventType, "downloadFolderImported") {
				continue
			}
			fileID, parseErr := strconv.Atoi(record.Data["fileId"])
			if parseErr == nil && fileID == file.ID && samePath(record.Data["droppedPath"], expectedSource) && samePath(record.Data["importedPath"], file.Path) {
				planned.Imported = true
				planned.EpisodeFileID = file.ID
				planned.LibraryPath = file.Path
				planned.HistoryID = record.ID
				verified = true
				break
			}
		}
		if !verified {
			complete = false
		}
	}
	return complete, nil
}

func (e *Engine) verifyRetirementGate(ctx context.Context, release Release) error {
	if release.CandidateRevision == nil {
		return fmt.Errorf("candidate revision is missing")
	}
	torrent, exists, err := e.torrentIfExists(ctx, release.CandidateRevision.TorrentID)
	if err != nil || !exists {
		return fmt.Errorf("new torrent is absent at retirement gate: %w", err)
	}
	if !torrent.ForceStart || !activeSeedingState(torrent.State) || torrent.Progress != 1 {
		return fmt.Errorf("new torrent is not complete, force-started, and actively seeding at retirement gate")
	}
	if err := e.verifyCandidateTorrent(ctx, torrent, release.CandidateRevision, true); err != nil {
		return err
	}
	properties, err := e.qbittorrent.Properties(ctx, release.CandidateRevision.TorrentID)
	if err != nil {
		return err
	}
	if properties.PiecesNum <= 0 || properties.PiecesHave != properties.PiecesNum {
		return fmt.Errorf("new torrent does not have every piece at retirement gate")
	}
	if err := e.verifyCandidateContent(ctx, release.CandidateRevision, false); err != nil {
		return err
	}
	complete, err := e.verifyImports(ctx, &release)
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("Sonarr import receipts are incomplete at retirement gate")
	}
	return nil
}

func (e *Engine) verifyCandidateContent(ctx context.Context, revision *Revision, establishMissing bool) error {
	for index := range revision.Files {
		file := &revision.Files[index]
		if file.EpisodeID == 0 {
			continue
		}
		digest, err := e.digestRemoteFile(ctx, path.Join(revision.SavePath, file.CurrentPath), file.Size)
		if err != nil {
			return fmt.Errorf("hash candidate source %q: %w", file.CurrentPath, err)
		}
		if file.ContentSHA256 == "" && establishMissing {
			file.ContentSHA256 = digest
		}
		if file.ContentSHA256 == "" || digest != file.ContentSHA256 {
			return fmt.Errorf("candidate source content changed for episode %d", file.EpisodeID)
		}
	}
	return nil
}

func (e *Engine) verifyRetainedOldFiles(ctx context.Context, revision Revision) error {
	for _, file := range revision.Files {
		if file.EpisodeID == 0 {
			continue
		}
		if file.ContentSHA256 == "" {
			return fmt.Errorf("old source has no content receipt for episode %d", file.EpisodeID)
		}
		digest, err := e.digestRemoteFile(ctx, path.Join(revision.SavePath, file.CurrentPath), file.Size)
		if err != nil {
			return fmt.Errorf("verify retained old source %q: %w", file.CurrentPath, err)
		}
		if digest != file.ContentSHA256 {
			return fmt.Errorf("retained old source content changed for episode %d", file.EpisodeID)
		}
	}
	return nil
}

func revisionFileByIndex(revision *Revision, index int) *RevisionFile {
	for position := range revision.Files {
		if revision.Files[position].Index == index {
			return &revision.Files[position]
		}
	}
	return nil
}

func qbitFilesByIndex(files []qbittorrent.File) map[int]qbittorrent.File {
	result := make(map[int]qbittorrent.File, len(files))
	for _, file := range files {
		result[file.Index] = file
	}
	return result
}

func normalizePath(value string) string {
	return strings.TrimRight(strings.ReplaceAll(value, `\`, "/"), "/")
}

func stoppedState(state string) bool {
	switch state {
	case "stoppedUP", "stoppedDL", "pausedUP", "pausedDL":
		return true
	default:
		return false
	}
}

func checkingState(state string) bool {
	return strings.HasPrefix(state, "checking") || state == "queuedForChecking" || state == "checkingResumeData"
}

func movingState(state string) bool {
	return state == "moving"
}

func errorState(state string) bool {
	return state == "error" || state == "missingFiles" || state == "unknown"
}

func activeSeedingState(state string) bool {
	switch state {
	case "uploading", "stalledUP", "forcedUP":
		return true
	default:
		return false
	}
}

func (e *Engine) commandPhaseExpired(operation *Operation) bool {
	return e.commandTimeout > 0 && time.Since(operation.UpdatedAt) >= e.commandTimeout
}

func sortedFileIndexes(files []RevisionFile) []int {
	result := make([]int, 0, len(files))
	for _, file := range files {
		result = append(result, file.Index)
	}
	sort.Ints(result)
	return result
}
