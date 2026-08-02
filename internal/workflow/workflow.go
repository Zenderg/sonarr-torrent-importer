package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zenderg/sonarr-torrent-importer/internal/mapper"
	"github.com/zenderg/sonarr-torrent-importer/internal/qbittorrent"
	"github.com/zenderg/sonarr-torrent-importer/internal/sonarr"
)

const minimumQBittorrentWebAPI = "2.11.0"

type sonarrAPI interface {
	SystemStatus(context.Context) (sonarr.SystemStatus, error)
	Queue(context.Context) ([]sonarr.QueueRecord, error)
	Episodes(context.Context, int, int) ([]sonarr.Episode, error)
	ManualImportCandidates(context.Context, string, string) ([]sonarr.ManualImportCandidate, error)
	Reprocess(context.Context, []sonarr.ManualImportReprocess) ([]sonarr.ManualImportReprocess, error)
	StartManualImport(context.Context, []sonarr.ManualImportFile) (sonarr.Command, error)
	Command(context.Context, int) (sonarr.Command, error)
	History(context.Context, string) ([]sonarr.HistoryRecord, error)
	EpisodeFiles(context.Context, []int) ([]sonarr.EpisodeFile, error)
	FinalizeQueue(context.Context, int, bool) error
}

type qbittorrentAPI interface {
	Login(context.Context) error
	Versions(context.Context) (qbittorrent.Versions, error)
	Torrent(context.Context, string) (qbittorrent.Torrent, error)
	Files(context.Context, string) ([]qbittorrent.File, error)
	RenameFile(context.Context, string, string, string) error
}

type Engine struct {
	sonarr          sonarrAPI
	qbittorrent     qbittorrentAPI
	commandTimeout  time.Duration
	workflowTimeout time.Duration
	pollInterval    time.Duration
	operations      *operationStore
}

func NewEngine(sonarrClient sonarrAPI, qbittorrentClient qbittorrentAPI, commandTimeout, workflowTimeout, pollInterval time.Duration, dataRoot string) (*Engine, error) {
	operations, err := newOperationStore(dataRoot)
	if err != nil {
		return nil, err
	}
	return &Engine{
		sonarr: sonarrClient, qbittorrent: qbittorrentClient,
		commandTimeout: commandTimeout, workflowTimeout: workflowTimeout, pollInterval: pollInterval,
		operations: operations,
	}, nil
}

func (e *Engine) LatestResult() (Result, bool, error) {
	record, exists, err := e.operations.loadLatest()
	if err != nil || !exists {
		return Result{}, exists, err
	}
	return record.Plan.Result, true, nil
}

func (e *Engine) Run(ctx context.Context, selection Selection, execute bool, expectedPlanToken string) (Result, error) {
	if !execute {
		built, err := e.buildPlan(ctx, selection, "dry-run")
		return built.result, err
	}
	if !validTorrentID(selection.DownloadID) {
		return blockedExecution(selection, "INVALID_DOWNLOAD_ID", "Execute requires the exact 40- or 64-character hexadecimal downloadId returned by dry-run."), nil
	}
	if expectedPlanToken == "" {
		return blockedExecution(selection, "PLAN_TOKEN_REQUIRED", "Execute requires the exact planToken returned by dry-run."), nil
	}
	lock, err := e.operations.tryLock()
	if errors.Is(err, errExecutionLocked) {
		return blockedExecution(selection, "EXECUTION_ALREADY_RUNNING", err.Error()), nil
	}
	if err != nil {
		return blockedExecution(selection, "EXECUTION_LOCK_FAILED", err.Error()), err
	}
	defer lock.Close()

	record, exists, err := e.operations.load(selection.DownloadID)
	if err != nil {
		return blockedExecution(selection, "OPERATION_STATE_INVALID", err.Error()), err
	}
	if exists {
		if record.PlanToken != expectedPlanToken {
			return blockedExecution(selection, "PLAN_TOKEN_MISMATCH", "A durable operation for this downloadId exists with a different planToken."), nil
		}
		mutationContext, cancel := context.WithTimeout(context.Background(), e.workflowTimeout)
		defer cancel()
		return e.resumeOperation(mutationContext, record)
	}

	built, err := e.buildPlan(ctx, selection, "execute")
	if err != nil {
		return built.result, err
	}
	if !built.result.CanExecute {
		return built.result, nil
	}
	if built.result.PlanToken != expectedPlanToken {
		block(&built.result, "DRY_RUN_PLAN_CHANGED", "The current verified plan does not match the supplied dry-run planToken.")
		built.result.CanExecute = false
		return built.result, nil
	}
	return e.execute(ctx, built)
}

func (e *Engine) buildPlan(ctx context.Context, selection Selection, mode string) (plan, error) {
	built := plan{
		selection: selection,
		result: Result{
			Mode: mode, Outcome: "blocked", Selection: selection,
			Timeline: []Event{},
		},
	}

	status, err := e.sonarr.SystemStatus(ctx)
	if err != nil {
		return built, fmt.Errorf("discover Sonarr version: %w", err)
	}
	built.result.Versions.Sonarr = status.Version
	if majorVersion(status.Version) != 4 {
		block(&built.result, "UNSUPPORTED_SONARR_VERSION", fmt.Sprintf("The importer supports Sonarr v4; server reported %q.", status.Version))
		return built, nil
	}
	event(&built.result, "capabilities", "ok", "Sonarr v4 capability boundary confirmed.")

	if err := e.qbittorrent.Login(ctx); err != nil {
		return built, fmt.Errorf("authenticate with qBittorrent: %w", err)
	}
	qbitVersions, err := e.qbittorrent.Versions(ctx)
	if err != nil {
		return built, fmt.Errorf("discover qBittorrent version: %w", err)
	}
	built.result.Versions.QBittorrent = qbitVersions.Application
	built.result.Versions.QBittorrentWebAPI = qbitVersions.WebAPI
	if majorVersion(qbitVersions.Application) != 5 || compareVersions(qbitVersions.WebAPI, minimumQBittorrentWebAPI) < 0 {
		block(&built.result, "UNSUPPORTED_QBITTORRENT_VERSION", fmt.Sprintf("Canonical source renaming requires qBittorrent v5 with WebAPI >= %s; server reported application %q and WebAPI %q.", minimumQBittorrentWebAPI, qbitVersions.Application, qbitVersions.WebAPI))
		return built, nil
	}
	event(&built.result, "capabilities", "ok", "qBittorrent WebAPI capability boundary confirmed.")

	queue, err := e.sonarr.Queue(ctx)
	if err != nil {
		return built, fmt.Errorf("read Sonarr queue: %w", err)
	}
	resolved, contextEvidence, outputPath, err := resolveQueueSelection(queue, selection)
	if err != nil {
		block(&built.result, "QUEUE_CONTEXT_NOT_CONFIRMED", err.Error())
		return built, nil
	}
	built.queueRecords = resolved
	built.context = contextEvidence
	built.outputPath = outputPath
	built.selection.DownloadID = contextEvidence.DownloadID
	built.result.Selection = built.selection
	built.result.Context = &built.context
	event(&built.result, "queue-context", "ok", fmt.Sprintf("Confirmed series %d season %d from %d Sonarr queue item(s).", contextEvidence.SeriesID, contextEvidence.SeasonNumber, len(resolved)))

	if !validTorrentID(contextEvidence.DownloadID) {
		block(&built.result, "INVALID_DOWNLOAD_ID", "The Sonarr download ID is not a 40- or 64-character hexadecimal torrent identity.")
		return built, nil
	}

	torrent, err := e.qbittorrent.Torrent(ctx, contextEvidence.DownloadID)
	if err != nil {
		return built, fmt.Errorf("read qBittorrent torrent: %w", err)
	}
	built.torrent = torrent
	if !activeSeedingState(torrent.State) {
		block(&built.result, "TORRENT_NOT_ACTIVE_SEEDING", fmt.Sprintf("Torrent state %q is not an active seeding state; the importer will not risk Sonarr finalizing a stopped completed download.", torrent.State))
		return built, nil
	}

	manifest, err := e.qbittorrent.Files(ctx, torrent.Hash)
	if err != nil {
		return built, fmt.Errorf("read qBittorrent manifest: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		block(&built.result, "INVALID_TORRENT_MANIFEST", err.Error())
		return built, nil
	}
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Index < manifest[j].Index })
	built.manifest = manifest
	built.manifestSHA256, err = manifestDigest(torrent, manifest)
	if err != nil {
		return built, err
	}
	built.result.ManifestSHA256 = built.manifestSHA256
	built.result.Torrent = &TorrentSummary{
		Hash: torrent.Hash, Name: torrent.Name, State: torrent.State,
		Category: torrent.Category, FileCount: len(manifest),
	}
	event(&built.result, "manifest", "ok", fmt.Sprintf("Captured candidate snapshot %s with %d files.", built.manifestSHA256, len(manifest)))

	mapFiles := make([]mapper.File, 0)
	mapResultIndexes := make([]int, 0)
	for _, manifestFile := range manifest {
		media := isMediaPath(manifestFile.Name)
		selected := manifestFile.Priority != 0
		complete := manifestFile.Progress == 1
		fileResult := FileResult{
			Index: manifestFile.Index, RelativePath: manifestFile.Name, Size: manifestFile.Size,
			Selected: selected, Complete: complete, Media: media,
		}
		switch {
		case !media:
			fileResult.Outcome = "ignored"
			fileResult.Reason = "NON_MEDIA_FILE"
		case !selected:
			fileResult.Outcome = "ignored"
			fileResult.Reason = "UNSELECTED_MEDIA_FILE"
		case !complete:
			fileResult.Outcome = "blocked"
			fileResult.Reason = "SELECTED_MEDIA_FILE_INCOMPLETE"
		case manifestFile.Size == 0:
			fileResult.Outcome = "blocked"
			fileResult.Reason = "SELECTED_MEDIA_FILE_EMPTY"
		default:
			fileResult.Outcome = "mapping"
			mapFiles = append(mapFiles, mapper.File{RelativePath: manifestFile.Name, Size: manifestFile.Size})
			mapResultIndexes = append(mapResultIndexes, len(built.result.Files))
		}
		built.result.Files = append(built.result.Files, fileResult)
	}
	if len(mapFiles) == 0 {
		block(&built.result, "NO_SELECTED_COMPLETE_MEDIA", "The torrent manifest has no selected, complete media files.")
		return built, nil
	}

	episodes, err := e.sonarr.Episodes(ctx, contextEvidence.SeriesID, contextEvidence.SeasonNumber)
	if err != nil {
		return built, fmt.Errorf("read Sonarr episodes: %w", err)
	}
	built.episodes = episodes
	mapperEpisodes := make([]mapper.Episode, 0, len(episodes))
	for _, episode := range episodes {
		mapperEpisodes = append(mapperEpisodes, mapper.Episode{
			ID: episode.ID, SeriesID: episode.SeriesID, SeasonNumber: episode.SeasonNumber,
			EpisodeNumber: episode.EpisodeNumber, Title: episode.Title,
		})
	}
	mapping := mapper.Map(mapFiles, contextEvidence, mapperEpisodes)
	for index, decision := range mapping.Decisions {
		resultIndex := mapResultIndexes[index]
		decisionCopy := decision
		built.result.Files[resultIndex].Mapping = &decisionCopy
		if decision.Status == "blocked" {
			built.result.Files[resultIndex].Outcome = "blocked"
			built.result.Files[resultIndex].Reason = decision.Reason
		} else {
			built.result.Files[resultIndex].Outcome = "correlating"
		}
	}
	if hasBlockedFile(built.result.Files) {
		event(&built.result, "mapping", "blocked", "At least one selected media file lacks an exact deterministic mapping.")
		return built, nil
	}
	event(&built.result, "mapping", "ok", fmt.Sprintf("Mapped %d selected media files with exact evidence.", len(mapFiles)))

	seriesTitle, qualityName, err := queueRenameMetadata(resolved)
	if err != nil {
		block(&built.result, "RENAME_METADATA_NOT_CONFIRMED", err.Error())
		return built, nil
	}
	episodeByID := episodesByID(episodes)
	for _, resultIndex := range mapResultIndexes {
		fileResult := &built.result.Files[resultIndex]
		manifestFile := manifestFileByIndex(manifest, fileResult.Index)
		episode, found := episodeByID[fileResult.Mapping.EpisodeIDs[0]]
		if !found {
			fileResult.Outcome = "blocked"
			fileResult.Reason = "EPISODE_METADATA_DISAPPEARED"
			continue
		}
		if episode.HasFile {
			fileResult.Outcome = "blocked"
			fileResult.Reason = "EPISODE_ALREADY_HAS_FILE"
			continue
		}
		targetPath, renameErr := canonicalTorrentPath(manifestFile.Name, seriesTitle, qualityName, episode)
		if renameErr != nil {
			fileResult.Outcome = "blocked"
			fileResult.Reason = "CANONICAL_RENAME_INVALID"
			continue
		}
		renameStatus := "planned"
		if targetPath == manifestFile.Name {
			renameStatus = "not_required"
		}
		fileResult.Rename = &RenameResult{FromPath: manifestFile.Name, ToPath: targetPath, Status: renameStatus}
		fileResult.Outcome = "ready"
		built.prepared = append(built.prepared, preparedFile{
			resultIndex: resultIndex,
			manifest:    manifestFile, originalPath: manifestFile.Name, targetPath: targetPath,
			candidate: sonarr.ManualImportCandidate{DownloadID: contextEvidence.DownloadID},
		})
	}
	if err := validateRenamePlan(manifest, built.prepared); err != nil {
		block(&built.result, "RENAME_PLAN_COLLISION", err.Error())
		return built, nil
	}
	for index := range built.prepared {
		expectedPath, pathErr := expectedSonarrSourcePath(outputPath, built.prepared[index], built.prepared)
		if pathErr != nil {
			block(&built.result, "SONARR_SOURCE_PATH_NOT_CONFIRMED", pathErr.Error())
			return built, nil
		}
		built.prepared[index].expectedSourcePath = expectedPath
	}
	if hasBlockedFile(built.result.Files) {
		event(&built.result, "rename-plan", "blocked", "At least one selected media file cannot be renamed safely.")
		return built, nil
	}
	event(&built.result, "rename-plan", "ok", fmt.Sprintf("Planned %d canonical qBittorrent file rename(s) from confirmed Sonarr metadata.", len(built.prepared)))

	history, err := e.sonarr.History(ctx, contextEvidence.DownloadID)
	if err != nil {
		return built, fmt.Errorf("read Sonarr import history: %w", err)
	}
	built.historyBaseline = make(map[int]struct{}, len(history))
	for _, record := range history {
		built.historyBaseline[record.ID] = struct{}{}
	}
	built.result.CanExecute = true
	built.result.Outcome = "ready"
	built.result.PlanToken, err = calculatePlanToken(built)
	if err != nil {
		return built, err
	}
	event(&built.result, "plan", "ok", "Dry-run evidence and canonical rename plan are complete; execute remains explicitly opt-in.")
	return built, nil
}

func calculatePlanToken(built plan) (string, error) {
	historyIDs := make([]int, 0, len(built.historyBaseline))
	for id := range built.historyBaseline {
		historyIDs = append(historyIDs, id)
	}
	sort.Ints(historyIDs)
	commandFiles := make([]sonarr.ManualImportFile, 0, len(built.prepared))
	for _, prepared := range built.prepared {
		commandFiles = append(commandFiles, prepared.commandFile)
	}
	payload := struct {
		Selection           Selection                 `json:"selection"`
		Context             mapper.Context            `json:"context"`
		OutputPath          string                    `json:"outputPath"`
		Queue               string                    `json:"queue"`
		Episodes            string                    `json:"episodes"`
		TorrentHash         string                    `json:"torrentHash"`
		TorrentName         string                    `json:"torrentName"`
		TorrentState        string                    `json:"torrentState"`
		TorrentCategory     string                    `json:"torrentCategory"`
		TorrentSavePath     string                    `json:"torrentSavePath"`
		TorrentContentPath  string                    `json:"torrentContentPath"`
		ManifestSHA256      string                    `json:"manifestSha256"`
		ExpectedSourcePaths []string                  `json:"expectedSourcePaths"`
		HistoryIDs          []int                     `json:"historyIds"`
		Files               []FileResult              `json:"files"`
		ManualImportFiles   []sonarr.ManualImportFile `json:"manualImportFiles"`
	}{
		Selection:          Selection{DownloadID: built.context.DownloadID},
		Context:            built.context,
		OutputPath:         normalizeSonarrPath(built.outputPath),
		Queue:              queueSafetySnapshot(built.queueRecords),
		Episodes:           episodeSnapshot(built.episodes),
		TorrentHash:        strings.ToLower(built.torrent.Hash),
		TorrentName:        built.torrent.Name,
		TorrentState:       strings.ToLower(built.torrent.State),
		TorrentCategory:    built.torrent.Category,
		TorrentSavePath:    built.torrent.SavePath,
		TorrentContentPath: built.torrent.ContentPath,
		ManifestSHA256:     built.manifestSHA256,
		HistoryIDs:         historyIDs,
		Files:              built.result.Files,
		ManualImportFiles:  commandFiles,
	}
	for _, prepared := range built.prepared {
		payload.ExpectedSourcePaths = append(payload.ExpectedSourcePaths, normalizeSonarrPath(prepared.expectedSourcePath))
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode dry-run plan token: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func blockedExecution(selection Selection, code, message string) Result {
	result := Result{
		Mode: "execute", Outcome: "blocked", Selection: selection,
		Timeline: []Event{},
	}
	block(&result, code, message)
	return result
}

func (e *Engine) reprocess(ctx context.Context, built *plan) error {
	requests := make([]sonarr.ManualImportReprocess, 0, len(built.prepared))
	preparedIndexes := make([]int, 0, len(built.prepared))
	for preparedIndex, prepared := range built.prepared {
		fileResult := &built.result.Files[prepared.resultIndex]
		if fileResult.Outcome == "imported" || fileResult.Outcome == "already_satisfied" {
			continue
		}
		candidate := prepared.candidate
		if !rawPresent(candidate.Quality) || !rawPresent(candidate.Languages) || !rawPresent(candidate.ReleaseType) {
			fileResult.Outcome = "blocked"
			fileResult.Reason = "SONARR_CANDIDATE_ATTRIBUTES_MISSING"
			continue
		}
		preparedIndexes = append(preparedIndexes, preparedIndex)
		requests = append(requests, sonarr.ManualImportReprocess{
			Path: candidate.Path, SeriesID: built.context.SeriesID,
			SeasonNumber: &built.context.SeasonNumber,
			EpisodeIDs:   append([]int(nil), built.result.Files[prepared.resultIndex].Mapping.EpisodeIDs...),
			Quality:      candidate.Quality, Languages: candidate.Languages,
			ReleaseGroup: candidate.ReleaseGroup, DownloadID: built.context.DownloadID,
			IndexerFlags: candidate.IndexerFlags, ReleaseType: candidate.ReleaseType,
		})
	}
	if hasBlockedFile(built.result.Files) || len(requests) == 0 {
		return nil
	}
	responses, err := e.sonarr.Reprocess(ctx, requests)
	if err != nil {
		return fmt.Errorf("reprocess explicit Sonarr mappings: %w", err)
	}
	if len(responses) != len(requests) {
		for _, preparedIndex := range preparedIndexes {
			prepared := built.prepared[preparedIndex]
			fileResult := &built.result.Files[prepared.resultIndex]
			fileResult.Outcome = "blocked"
			fileResult.Reason = "SONARR_REPROCESS_RESPONSE_MISMATCH"
		}
		return nil
	}
	byPath := make(map[string]sonarr.ManualImportReprocess, len(responses))
	duplicatePaths := make(map[string]struct{})
	for _, response := range responses {
		if _, exists := byPath[response.Path]; exists {
			duplicatePaths[response.Path] = struct{}{}
		}
		byPath[response.Path] = response
	}
	for _, preparedIndex := range preparedIndexes {
		prepared := &built.prepared[preparedIndex]
		fileResult := &built.result.Files[prepared.resultIndex]
		response, found := byPath[prepared.candidate.Path]
		if _, duplicate := duplicatePaths[prepared.candidate.Path]; !found || duplicate {
			fileResult.Outcome = "blocked"
			fileResult.Reason = "SONARR_REPROCESS_RESPONSE_MISMATCH"
			continue
		}
		fileResult.Rejections = response.Rejections
		if len(response.Rejections) > 0 {
			fileResult.Outcome = "blocked"
			fileResult.Reason = "SONARR_REPROCESS_REJECTED"
			continue
		}
		if response.SeriesID != built.context.SeriesID || response.SeasonNumber == nil || *response.SeasonNumber != built.context.SeasonNumber || !strings.EqualFold(response.DownloadID, built.context.DownloadID) {
			fileResult.Outcome = "blocked"
			fileResult.Reason = "SONARR_REPROCESS_CONTEXT_CHANGED"
			continue
		}
		responseEpisodeIDs := make([]int, 0, len(response.Episodes))
		for _, episode := range response.Episodes {
			if episode.SeriesID != built.context.SeriesID || episode.SeasonNumber != built.context.SeasonNumber {
				fileResult.Outcome = "blocked"
				fileResult.Reason = "SONARR_REPROCESS_CONTEXT_CHANGED"
				break
			}
			responseEpisodeIDs = append(responseEpisodeIDs, episode.ID)
		}
		sort.Ints(responseEpisodeIDs)
		expectedIDs := append([]int(nil), fileResult.Mapping.EpisodeIDs...)
		sort.Ints(expectedIDs)
		if fileResult.Outcome == "blocked" || !equalInts(responseEpisodeIDs, expectedIDs) {
			fileResult.Outcome = "blocked"
			fileResult.Reason = "SONARR_REPROCESS_MAPPING_CHANGED"
			continue
		}
		if !rawPresent(response.Quality) || !rawPresent(response.Languages) || !rawPresent(response.ReleaseType) {
			fileResult.Outcome = "blocked"
			fileResult.Reason = "SONARR_REPROCESS_ATTRIBUTES_MISSING"
			continue
		}
		prepared.commandFile = sonarr.ManualImportFile{
			Path: response.Path, FolderName: prepared.candidate.FolderName,
			SeriesID: built.context.SeriesID, EpisodeIDs: expectedIDs,
			Quality: response.Quality, Languages: response.Languages,
			ReleaseGroup: response.ReleaseGroup, IndexerFlags: response.IndexerFlags,
			ReleaseType: response.ReleaseType, DownloadID: built.context.DownloadID,
		}
		fileResult.Outcome = "ready"
	}
	return nil
}

func resolveQueueSelection(records []sonarr.QueueRecord, selection Selection) ([]sonarr.QueueRecord, mapper.Context, string, error) {
	downloadID := strings.TrimSpace(selection.DownloadID)
	if selection.QueueID > 0 {
		var selected *sonarr.QueueRecord
		for index := range records {
			if records[index].ID == selection.QueueID {
				selected = &records[index]
				break
			}
		}
		if selected == nil {
			return nil, mapper.Context{}, "", fmt.Errorf("Sonarr queue item %d was not found", selection.QueueID)
		}
		if downloadID != "" && !strings.EqualFold(downloadID, selected.DownloadID) {
			return nil, mapper.Context{}, "", fmt.Errorf("queue item %d has a different download ID", selection.QueueID)
		}
		downloadID = selected.DownloadID
	}
	if downloadID == "" {
		return nil, mapper.Context{}, "", fmt.Errorf("downloadId or queueId is required")
	}

	matched := make([]sonarr.QueueRecord, 0)
	seriesIDs := map[int]struct{}{}
	seasonNumbers := map[int]struct{}{}
	outputPaths := map[string]string{}
	downloadClients := map[string]struct{}{}
	postImportCategory := map[bool]struct{}{}
	for _, record := range records {
		if !strings.EqualFold(record.DownloadID, downloadID) {
			continue
		}
		if !strings.EqualFold(record.Protocol, "torrent") {
			return nil, mapper.Context{}, "", fmt.Errorf("queue download %q is not a torrent", downloadID)
		}
		matched = append(matched, record)
		id, hasSeries := queueSeriesID(record)
		season, hasSeason := queueSeasonNumber(record)
		if !hasSeries || !hasSeason || record.OutputPath == "" || record.DownloadClient == "" {
			return nil, mapper.Context{}, "", fmt.Errorf("queue item %d lacks required series, season, output path, or download client context", record.ID)
		}
		seriesIDs[id] = struct{}{}
		seasonNumbers[season] = struct{}{}
		outputPaths[normalizeSonarrPath(record.OutputPath)] = record.OutputPath
		downloadClients[record.DownloadClient] = struct{}{}
		postImportCategory[record.DownloadClientHasPostImportCategory] = struct{}{}
	}
	if len(matched) == 0 {
		return nil, mapper.Context{}, "", fmt.Errorf("no current Sonarr queue items match download %q", downloadID)
	}
	if len(seriesIDs) != 1 || len(seasonNumbers) != 1 {
		return nil, mapper.Context{}, "", fmt.Errorf("queue items do not provide one unambiguous series and season")
	}
	if len(outputPaths) != 1 {
		return nil, mapper.Context{}, "", fmt.Errorf("queue items do not provide one unambiguous output path")
	}
	if len(downloadClients) != 1 || len(postImportCategory) != 1 {
		return nil, mapper.Context{}, "", fmt.Errorf("queue items do not provide one unambiguous download-client finalization policy")
	}
	seriesID := onlyMapKey(seriesIDs)
	seasonNumber := onlyMapKey(seasonNumbers)
	queueIDs := make([]int, 0, len(matched))
	for _, record := range matched {
		queueIDs = append(queueIDs, record.ID)
	}
	sort.Ints(queueIDs)
	return matched, mapper.Context{
		SeriesID: seriesID, SeasonNumber: seasonNumber, QueueIDs: queueIDs,
		DownloadID: matched[0].DownloadID, Source: "sonarrQueue",
	}, onlyStringMapValue(outputPaths), nil
}

func queueSeriesID(record sonarr.QueueRecord) (int, bool) {
	if record.SeriesID != nil && *record.SeriesID > 0 {
		return *record.SeriesID, true
	}
	if record.Series != nil && record.Series.ID > 0 {
		return record.Series.ID, true
	}
	return 0, false
}

func queueSeasonNumber(record sonarr.QueueRecord) (int, bool) {
	if record.SeasonNumber != nil && *record.SeasonNumber >= 0 {
		return *record.SeasonNumber, true
	}
	if record.Episode != nil && record.Episode.SeasonNumber >= 0 {
		return record.Episode.SeasonNumber, true
	}
	return 0, false
}

func correlateCandidate(file qbittorrent.File, candidates []sonarr.ManualImportCandidate, context mapper.Context, outputPath string) (sonarr.ManualImportCandidate, error) {
	matches := make([]sonarr.ManualImportCandidate, 0)
	for _, candidate := range candidates {
		if candidate.Path == "" || candidate.RelativePath == "" || candidate.Size != file.Size {
			continue
		}
		if !strings.EqualFold(candidate.DownloadID, context.DownloadID) || candidate.Series == nil || candidate.Series.ID != context.SeriesID {
			continue
		}
		if candidate.SeasonNumber != nil && *candidate.SeasonNumber != context.SeasonNumber {
			continue
		}
		if !pathWithin(candidate.Path, outputPath) {
			continue
		}
		candidateRelative := normalizeRelativeForCorrelation(candidate.RelativePath)
		qbitRelative := normalizeRelativeForCorrelation(file.Name)
		if sameRelativePath(qbitRelative, candidateRelative) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) != 1 {
		return sonarr.ManualImportCandidate{}, fmt.Errorf("expected one Sonarr candidate for %q, got %d", file.Name, len(matches))
	}
	return matches[0], nil
}

func validateManifest(files []qbittorrent.File) error {
	if len(files) == 0 {
		return fmt.Errorf("qBittorrent returned an empty manifest")
	}
	indexes := make(map[int]struct{}, len(files))
	paths := make(map[string]struct{}, len(files))
	foldedPaths := make(map[string]string, len(files))
	for _, file := range files {
		if file.Index < 0 {
			return fmt.Errorf("manifest has a negative file index")
		}
		if _, exists := indexes[file.Index]; exists {
			return fmt.Errorf("manifest repeats file index %d", file.Index)
		}
		indexes[file.Index] = struct{}{}
		if _, exists := paths[file.Name]; exists {
			return fmt.Errorf("manifest repeats relative path %q", file.Name)
		}
		paths[file.Name] = struct{}{}
		folded := strings.ToLower(file.Name)
		if previous, exists := foldedPaths[folded]; exists {
			return fmt.Errorf("manifest paths %q and %q differ only by case", previous, file.Name)
		}
		foldedPaths[folded] = file.Name
		if !validTorrentPath(file.Name) || file.Size < 0 || file.Priority < 0 || math.IsNaN(file.Progress) || math.IsInf(file.Progress, 0) || file.Progress < 0 || file.Progress > 1 {
			return fmt.Errorf("manifest file %d has invalid metadata", file.Index)
		}
	}
	return nil
}

func validTorrentPath(value string) bool {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || strings.HasPrefix(value, "/") || strings.Contains(value, `\`) || strings.ContainsRune(value, '\x00') || path.Clean(value) != value || value == "." {
		return false
	}
	if len(value) >= 2 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, character := range segment {
			if unicode.IsControl(character) {
				return false
			}
		}
	}
	return true
}

func queueRenameMetadata(records []sonarr.QueueRecord) (string, string, error) {
	seriesTitles := make(map[string]string)
	qualityNames := make(map[string]string)
	for _, record := range records {
		if record.Series == nil || strings.TrimSpace(record.Series.Title) == "" {
			return "", "", fmt.Errorf("queue item %d lacks a series title required for canonical filenames", record.ID)
		}
		seriesTitle := strings.TrimSpace(record.Series.Title)
		seriesTitles[strings.ToLower(seriesTitle)] = seriesTitle
		qualityName, err := queueQualityName(record.Quality)
		if err != nil {
			return "", "", fmt.Errorf("queue item %d: %w", record.ID, err)
		}
		qualityNames[strings.ToLower(qualityName)] = qualityName
	}
	if len(seriesTitles) != 1 || len(qualityNames) != 1 {
		return "", "", fmt.Errorf("queue items do not agree on one series title and quality")
	}
	return onlyStringMapValue(seriesTitles), onlyStringMapValue(qualityNames), nil
}

func queueQualityName(raw json.RawMessage) (string, error) {
	var value struct {
		Quality struct {
			Name string `json:"name"`
		} `json:"quality"`
	}
	if !rawPresent(raw) || json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value.Quality.Name) == "" {
		return "", fmt.Errorf("quality metadata is missing or invalid")
	}
	return strings.TrimSpace(value.Quality.Name), nil
}

func canonicalTorrentPath(source, seriesTitle, qualityName string, episode sonarr.Episode) (string, error) {
	if !validTorrentPath(source) || episode.SeasonNumber < 0 || episode.EpisodeNumber <= 0 {
		return "", fmt.Errorf("source path or episode metadata is invalid")
	}
	if path.Dir(source) == "." {
		return "", fmt.Errorf("single-file torrent rename is not supported because the Sonarr output path may become stale")
	}
	extension := path.Ext(source)
	if !isMediaPath("file" + extension) {
		return "", fmt.Errorf("source extension %q is not supported media", extension)
	}
	seriesToken := canonicalFilenameToken(seriesTitle)
	qualityToken := canonicalFilenameToken(qualityName)
	if seriesToken == "" || qualityToken == "" {
		return "", fmt.Errorf("series title or quality cannot form a canonical filename")
	}
	base := fmt.Sprintf("%s.S%02dE%02d.%s%s", seriesToken, episode.SeasonNumber, episode.EpisodeNumber, qualityToken, extension)
	if len(base) > 240 || strings.HasSuffix(base, ".") || strings.HasSuffix(base, " ") {
		return "", fmt.Errorf("canonical filename is not safe for qBittorrent")
	}
	target := base
	if parent := path.Dir(source); parent != "." {
		target = parent + "/" + base
	}
	if !validTorrentPath(target) || path.Dir(target) != path.Dir(source) {
		return "", fmt.Errorf("canonical target is not a safe same-directory path")
	}
	return target, nil
}

func canonicalFilenameToken(value string) string {
	var builder strings.Builder
	separator := false
	for _, character := range strings.TrimSpace(value) {
		switch {
		case unicode.IsLetter(character) || unicode.IsDigit(character):
			if separator && builder.Len() > 0 {
				builder.WriteByte('.')
			}
			separator = false
			builder.WriteRune(character)
		case character == '-':
			if builder.Len() > 0 && !separator {
				builder.WriteRune(character)
			}
		default:
			separator = true
		}
	}
	return strings.Trim(builder.String(), ".-")
}

func validateRenamePlan(manifest []qbittorrent.File, prepared []preparedFile) error {
	occupied := make(map[string]qbittorrent.File, len(manifest))
	sources := make(map[string]struct{}, len(prepared))
	for _, file := range manifest {
		occupied[strings.ToLower(file.Name)] = file
	}
	for _, file := range prepared {
		sources[strings.ToLower(file.originalPath)] = struct{}{}
	}
	targets := make(map[string]string, len(prepared))
	for _, file := range prepared {
		if path.Dir(file.originalPath) != path.Dir(file.targetPath) {
			return fmt.Errorf("rename %q to %q would change directories", file.originalPath, file.targetPath)
		}
		foldedSource := strings.ToLower(file.originalPath)
		foldedTarget := strings.ToLower(file.targetPath)
		if foldedSource == foldedTarget && file.originalPath != file.targetPath {
			return fmt.Errorf("case-only rename %q to %q is not supported", file.originalPath, file.targetPath)
		}
		if previous, exists := targets[foldedTarget]; exists && previous != file.targetPath {
			return fmt.Errorf("rename targets %q and %q collide", previous, file.targetPath)
		}
		if _, exists := targets[foldedTarget]; exists {
			return fmt.Errorf("rename target %q is repeated", file.targetPath)
		}
		targets[foldedTarget] = file.targetPath
		if foldedTarget == foldedSource && file.targetPath == file.originalPath {
			continue
		}
		if occupiedFile, exists := occupied[foldedTarget]; exists {
			return fmt.Errorf("rename target %q is already occupied by manifest file %d", file.targetPath, occupiedFile.Index)
		}
		if _, isAnotherSource := sources[foldedTarget]; isAnotherSource {
			return fmt.Errorf("rename target %q overlaps another rename source", file.targetPath)
		}
	}
	return nil
}

func manifestDigest(torrent qbittorrent.Torrent, files []qbittorrent.File) (string, error) {
	type fileSnapshot struct {
		Index    int     `json:"index"`
		Name     string  `json:"name"`
		Size     int64   `json:"size"`
		Progress float64 `json:"progress"`
		Priority int     `json:"priority"`
	}
	snapshot := struct {
		Hash     string         `json:"hash"`
		Name     string         `json:"name"`
		SavePath string         `json:"savePath"`
		Files    []fileSnapshot `json:"files"`
	}{
		Hash: strings.ToLower(torrent.Hash), Name: torrent.Name,
		SavePath: torrent.SavePath,
		Files:    make([]fileSnapshot, 0, len(files)),
	}
	for _, file := range files {
		snapshot.Files = append(snapshot.Files, fileSnapshot{
			Index: file.Index, Name: file.Name, Size: file.Size,
			Progress: file.Progress, Priority: file.Priority,
		})
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("encode torrent manifest snapshot: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func activeSeedingState(state string) bool {
	switch strings.ToLower(state) {
	case "uploading", "stalledup", "forcedup":
		return true
	default:
		return false
	}
}

func isMediaPath(value string) bool {
	switch strings.ToLower(path.Ext(value)) {
	case ".mkv", ".mp4", ".m4v", ".avi", ".mov", ".wmv", ".webm", ".ts", ".m2ts":
		return true
	default:
		return false
	}
}

func sameRelativePath(left, right string) bool {
	return left == right || strings.HasSuffix(left, "/"+right) || strings.HasSuffix(right, "/"+left)
}

func normalizeRelativeForCorrelation(value string) string {
	return strings.TrimPrefix(strings.ReplaceAll(value, `\`, "/"), "./")
}

func normalizeSonarrPath(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")
	return strings.TrimRight(value, "/")
}

func pathWithin(candidate, root string) bool {
	candidate = normalizeSonarrPath(candidate)
	root = normalizeSonarrPath(root)
	if candidate == "" || root == "" {
		return false
	}
	if candidate == root {
		return true
	}
	return strings.HasPrefix(candidate, root+"/")
}

func validTorrentID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func rawPresent(value json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(value))
	return trimmed != "" && trimmed != "null" && json.Valid(value)
}

func majorVersion(value string) int {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	major, _ := strconv.Atoi(strings.Split(value, ".")[0])
	return major
}

func compareVersions(left, right string) int {
	leftParts := strings.Split(strings.TrimPrefix(strings.TrimSpace(left), "v"), ".")
	rightParts := strings.Split(strings.TrimPrefix(strings.TrimSpace(right), "v"), ".")
	length := len(leftParts)
	if len(rightParts) > length {
		length = len(rightParts)
	}
	for index := 0; index < length; index++ {
		var leftValue, rightValue int
		if index < len(leftParts) {
			leftValue, _ = strconv.Atoi(leftParts[index])
		}
		if index < len(rightParts) {
			rightValue, _ = strconv.Atoi(rightParts[index])
		}
		if leftValue < rightValue {
			return -1
		}
		if leftValue > rightValue {
			return 1
		}
	}
	return 0
}

func block(result *Result, code, message string) {
	result.Outcome = "blocked"
	result.CanExecute = false
	result.Blockers = append(result.Blockers, Blocker{Code: code, Message: message})
	event(result, "safety-check", "blocked", message)
}

func event(result *Result, step, status, message string) {
	result.Timeline = append(result.Timeline, Event{Step: step, Status: status, Message: message})
}

func hasBlockedFile(files []FileResult) bool {
	for _, file := range files {
		if file.Outcome == "blocked" {
			return true
		}
	}
	return false
}

func onlyMapKey(values map[int]struct{}) int {
	for value := range values {
		return value
	}
	return 0
}

func onlyStringMapValue(values map[string]string) string {
	for _, value := range values {
		return value
	}
	return ""
}

func manifestFileByIndex(files []qbittorrent.File, index int) qbittorrent.File {
	for _, file := range files {
		if file.Index == index {
			return file
		}
	}
	return qbittorrent.File{}
}

func equalInts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
