package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zenderg/sonarr-torrent-importer/internal/mapper"
	"github.com/zenderg/sonarr-torrent-importer/internal/qbittorrent"
	"github.com/zenderg/sonarr-torrent-importer/internal/sonarr"
)

type safetySonarr struct {
	queue          []sonarr.QueueRecord
	episodes       []sonarr.Episode
	history        []sonarr.HistoryRecord
	episodeFiles   []sonarr.EpisodeFile
	startCalls     int
	finalizeCalls  int
	finalize       func(int, bool) error
	candidateCalls int
	candidates     func(int, string, string) ([]sonarr.ManualImportCandidate, error)
	reprocess      func([]sonarr.ManualImportReprocess) ([]sonarr.ManualImportReprocess, error)
}

func (s *safetySonarr) SystemStatus(context.Context) (sonarr.SystemStatus, error) {
	return sonarr.SystemStatus{Version: "4.0.0"}, nil
}
func (s *safetySonarr) Queue(context.Context) ([]sonarr.QueueRecord, error) {
	return append([]sonarr.QueueRecord(nil), s.queue...), nil
}
func (s *safetySonarr) Episodes(context.Context, int, int) ([]sonarr.Episode, error) {
	return append([]sonarr.Episode(nil), s.episodes...), nil
}

func (s *safetySonarr) ManualImportCandidates(_ context.Context, path, downloadID string) ([]sonarr.ManualImportCandidate, error) {
	s.candidateCalls++
	if s.candidates != nil {
		return s.candidates(s.candidateCalls, path, downloadID)
	}
	return nil, errors.New("unexpected ManualImportCandidates call")
}

func (s *safetySonarr) Reprocess(_ context.Context, requests []sonarr.ManualImportReprocess) ([]sonarr.ManualImportReprocess, error) {
	if s.reprocess != nil {
		return s.reprocess(requests)
	}
	return nil, errors.New("unexpected Reprocess call")
}
func (s *safetySonarr) StartManualImport(context.Context, []sonarr.ManualImportFile) (sonarr.Command, error) {
	s.startCalls++
	return sonarr.Command{}, errors.New("unexpected duplicate ManualImport")
}
func (s *safetySonarr) Command(context.Context, int) (sonarr.Command, error) {
	return sonarr.Command{}, errors.New("unexpected Command call")
}
func (s *safetySonarr) History(context.Context, string) ([]sonarr.HistoryRecord, error) {
	return append([]sonarr.HistoryRecord(nil), s.history...), nil
}
func (s *safetySonarr) EpisodeFiles(context.Context, []int) ([]sonarr.EpisodeFile, error) {
	return append([]sonarr.EpisodeFile(nil), s.episodeFiles...), nil
}
func (s *safetySonarr) FinalizeQueue(_ context.Context, id int, changeCategory bool) error {
	s.finalizeCalls++
	if s.finalize != nil {
		return s.finalize(id, changeCategory)
	}
	return nil
}

type safetyQBittorrent struct {
	torrent      qbittorrent.Torrent
	files        []qbittorrent.File
	renameCalls  int
	rename       func(string, string, string) error
	torrentCalls int
	torrentFn    func(int) qbittorrent.Torrent
}

func (q *safetyQBittorrent) Login(context.Context) error { return nil }
func (q *safetyQBittorrent) Versions(context.Context) (qbittorrent.Versions, error) {
	return qbittorrent.Versions{Application: "5.0", WebAPI: "2.8.2"}, nil
}
func (q *safetyQBittorrent) Torrent(context.Context, string) (qbittorrent.Torrent, error) {
	q.torrentCalls++
	if q.torrentFn != nil {
		return q.torrentFn(q.torrentCalls), nil
	}
	return q.torrent, nil
}
func (q *safetyQBittorrent) Files(context.Context, string) ([]qbittorrent.File, error) {
	return append([]qbittorrent.File(nil), q.files...), nil
}
func (q *safetyQBittorrent) RenameFile(_ context.Context, hash, oldPath, newPath string) error {
	q.renameCalls++
	if q.rename != nil {
		return q.rename(hash, oldPath, newPath)
	}
	return errors.New("unexpected RenameFile call")
}

func TestOperationStoreLockAndLatestRecord(t *testing.T) {
	store, err := newOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.tryLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.tryLock(); !errors.Is(err, errExecutionLocked) {
		t.Fatalf("second lock error = %v, want errExecutionLocked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := store.tryLock()
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	_ = second.Close()

	record := operationRecord{PlanToken: "sha256:test", Phase: "prepared", Plan: persistedPlan{Result: Result{Outcome: "ready"}}}
	if err := store.save(testDownloadID, record); err != nil {
		t.Fatal(err)
	}
	latest, exists, err := store.loadLatest()
	if err != nil || !exists || latest.Phase != "prepared" || latest.Plan.Result.Outcome != "ready" {
		t.Fatalf("latest record mismatch: exists=%v record=%+v err=%v", exists, latest, err)
	}
}

func TestUncertainManualImportResumesWithoutResubmission(t *testing.T) {
	store, err := newOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	season := 2
	seriesID := 101
	episodeID := 2203
	episodeFileID := 700
	manifest := qbittorrent.File{Index: 0, Name: "Clockwork Garden/[03].mkv", Size: 100, Progress: 1, Priority: 1}
	torrent := qbittorrent.Torrent{Hash: testDownloadID, Name: "Clockwork Garden", State: "uploading", Category: "sonarr"}
	digest, err := manifestDigest(torrent, []qbittorrent.File{manifest})
	if err != nil {
		t.Fatal(err)
	}
	candidate := sonarr.ManualImportCandidate{Path: "/downloads/Clockwork Garden/[03].mkv", DownloadID: testDownloadID}
	built := plan{
		result: Result{
			Mode: "execute", Outcome: "ready", CanExecute: true, PlanToken: "sha256:resume",
			Files: []FileResult{{
				Index: 0, RelativePath: manifest.Name, Size: manifest.Size, Outcome: "ready",
				Mapping: &mapper.Decision{EpisodeIDs: []int{episodeID}},
			}},
		},
		selection: Selection{DownloadID: testDownloadID},
		context:   mapper.Context{SeriesID: seriesID, SeasonNumber: season, QueueIDs: []int{11}, DownloadID: testDownloadID, Source: "sonarrQueue"},
		torrent:   torrent, manifest: []qbittorrent.File{manifest}, manifestSHA256: digest,
		historyBaseline: map[int]struct{}{},
		prepared:        []preparedFile{{resultIndex: 0, manifest: manifest, expectedSourcePath: candidate.Path, candidate: candidate}},
	}
	if err := store.save(testDownloadID, operationRecord{
		PlanToken: built.result.PlanToken,
		Phase:     "manual_import_submitting",
		Plan:      persistPlan(built),
	}); err != nil {
		t.Fatal(err)
	}

	sonarrAPI := &safetySonarr{
		episodes:     []sonarr.Episode{{ID: episodeID, SeriesID: seriesID, SeasonNumber: season, HasFile: true, EpisodeFileID: episodeFileID}},
		episodeFiles: []sonarr.EpisodeFile{{ID: episodeFileID, SeriesID: seriesID, SeasonNumber: season, Path: "/library/Clockwork Garden/Season 02/E03.mkv", Size: manifest.Size}},
		history: []sonarr.HistoryRecord{{
			ID: 900, EpisodeID: episodeID, SeriesID: seriesID, DownloadID: testDownloadID, EventType: "downloadFolderImported",
			Data: map[string]string{"droppedPath": candidate.Path, "fileId": "700", "importedPath": "/library/Clockwork Garden/Season 02/E03.mkv"},
		}},
	}
	qbitAPI := &safetyQBittorrent{torrent: torrent, files: []qbittorrent.File{manifest}}
	engine := &Engine{
		sonarr: sonarrAPI, qbittorrent: qbitAPI, operations: store,
		commandTimeout: time.Second, workflowTimeout: 2 * time.Second, pollInterval: time.Millisecond,
	}

	result, err := engine.Run(context.Background(), Selection{DownloadID: testDownloadID}, true, built.result.PlanToken)
	if err != nil {
		t.Fatal(err)
	}
	if sonarrAPI.startCalls != 0 {
		t.Fatalf("ManualImport was submitted %d times during recovery", sonarrAPI.startCalls)
	}
	if result.Outcome != "imported" || result.OperationPhase != "complete" {
		encoded, _ := json.Marshal(result)
		t.Fatalf("unexpected recovered result: %s", encoded)
	}
}

func TestManualImportReadyResumeReconcilesAutoImportBeforeSubmission(t *testing.T) {
	store, err := newOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	season, seriesID, episodeID, episodeFileID := 1, 101, 1101, 700
	manifest := qbittorrent.File{Index: 0, Name: "Release/Futurama.S01E01.WEBDL-720p.mkv", Size: 100, Progress: 1, Priority: 1}
	torrent := qbittorrent.Torrent{Hash: testDownloadID, Name: "Release", State: "stalledUP", Category: "sonarr"}
	digest, err := manifestDigest(torrent, []qbittorrent.File{manifest})
	if err != nil {
		t.Fatal(err)
	}
	candidate := sonarr.ManualImportCandidate{Path: "/downloads/Release/Futurama.S01E01.WEBDL-720p.mkv", DownloadID: testDownloadID}
	commandFile := sonarr.ManualImportFile{Path: candidate.Path, SeriesID: seriesID, EpisodeIDs: []int{episodeID}, DownloadID: testDownloadID}
	built := plan{
		result: Result{
			Mode: "execute", Outcome: "ready", CanExecute: true, PlanToken: "sha256:manual-ready-resume",
			Files: []FileResult{{
				Index: 0, RelativePath: manifest.Name, Size: manifest.Size, Outcome: "ready",
				Mapping: &mapper.Decision{EpisodeIDs: []int{episodeID}},
			}},
		},
		selection: Selection{DownloadID: testDownloadID},
		context:   mapper.Context{SeriesID: seriesID, SeasonNumber: season, DownloadID: testDownloadID, Source: "sonarrQueue"},
		torrent:   torrent, manifest: []qbittorrent.File{manifest}, manifestSHA256: digest,
		historyBaseline: map[int]struct{}{},
		prepared: []preparedFile{{
			resultIndex: 0, manifest: manifest, originalPath: manifest.Name, targetPath: manifest.Name,
			renameApplied: true, expectedSourcePath: candidate.Path, candidate: candidate, commandFile: commandFile,
		}},
	}
	if err := store.save(testDownloadID, operationRecord{
		PlanToken: built.result.PlanToken,
		Phase:     "manual_import_ready",
		Plan:      persistPlan(built),
	}); err != nil {
		t.Fatal(err)
	}

	sonarrAPI := &safetySonarr{
		episodes:     []sonarr.Episode{{ID: episodeID, SeriesID: seriesID, SeasonNumber: season, HasFile: true, EpisodeFileID: episodeFileID}},
		episodeFiles: []sonarr.EpisodeFile{{ID: episodeFileID, SeriesID: seriesID, SeasonNumber: season, Path: "/library/Futurama/Season 01/E01.mkv", Size: manifest.Size}},
		history: []sonarr.HistoryRecord{{
			ID: 900, EpisodeID: episodeID, SeriesID: seriesID, DownloadID: testDownloadID, EventType: "downloadFolderImported",
			Data: map[string]string{"droppedPath": candidate.Path, "fileId": "700", "importedPath": "/library/Futurama/Season 01/E01.mkv"},
		}},
	}
	engine := &Engine{
		sonarr: sonarrAPI, qbittorrent: &safetyQBittorrent{torrent: torrent, files: []qbittorrent.File{manifest}}, operations: store,
		commandTimeout: time.Second, workflowTimeout: 2 * time.Second, pollInterval: time.Millisecond,
	}

	result, err := engine.Run(context.Background(), Selection{DownloadID: testDownloadID}, true, built.result.PlanToken)
	if err != nil {
		t.Fatal(err)
	}
	if sonarrAPI.startCalls != 0 {
		t.Fatalf("ManualImport was submitted %d times after auto-import completed during downtime", sonarrAPI.startCalls)
	}
	if result.Outcome != "imported" || result.OperationPhase != "complete" {
		encoded, _ := json.Marshal(result)
		t.Fatalf("unexpected recovered result: %s", encoded)
	}
}

func TestQueueDeleteTransportErrorReconcilesByObservation(t *testing.T) {
	store, err := newOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seriesID, season := 101, 2
	record := sonarr.QueueRecord{
		ID: 11, SeriesID: &seriesID, SeasonNumber: &season, DownloadID: testDownloadID,
		Protocol: "torrent", OutputPath: "/downloads/Clockwork Garden", DownloadClient: "qBittorrent",
	}
	manifest := qbittorrent.File{Index: 0, Name: "[03].mkv", Size: 100, Progress: 1, Priority: 1}
	torrent := qbittorrent.Torrent{Hash: testDownloadID, Name: "Clockwork Garden", State: "uploading", Category: "sonarr"}
	digest, err := manifestDigest(torrent, []qbittorrent.File{manifest})
	if err != nil {
		t.Fatal(err)
	}
	sonarrAPI := &safetySonarr{queue: []sonarr.QueueRecord{record}}
	sonarrAPI.finalize = func(id int, _ bool) error {
		if id != record.ID {
			t.Fatalf("finalized queue ID %d, want %d", id, record.ID)
		}
		sonarrAPI.queue = nil
		return errors.New("response lost")
	}
	engine := &Engine{
		sonarr:      sonarrAPI,
		qbittorrent: &safetyQBittorrent{torrent: torrent, files: []qbittorrent.File{manifest}},
		operations:  store,
	}
	built := plan{
		result:     Result{Mode: "execute", Outcome: "ready", PlanToken: "sha256:delete"},
		selection:  Selection{DownloadID: testDownloadID},
		context:    mapper.Context{SeriesID: seriesID, SeasonNumber: season, QueueIDs: []int{record.ID}, DownloadID: testDownloadID, Source: "sonarrQueue"},
		outputPath: record.OutputPath, queueRecords: []sonarr.QueueRecord{record},
		torrent: torrent, manifest: []qbittorrent.File{manifest}, manifestSHA256: digest,
		historyBaseline: map[int]struct{}{},
	}

	result, err := engine.completeOperation(context.Background(), &built)
	if err != nil {
		t.Fatal(err)
	}
	if sonarrAPI.finalizeCalls != 1 || result.QueueFinalization == nil || result.QueueFinalization.Status != "verified" {
		t.Fatalf("uncertain DELETE was not reconciled: calls=%d result=%+v", sonarrAPI.finalizeCalls, result.QueueFinalization)
	}
}

func TestPendingQueueDeleteIsNotRepeatedWhileItemRemains(t *testing.T) {
	seriesID, season := 101, 2
	record := sonarr.QueueRecord{
		ID: 11, SeriesID: &seriesID, SeasonNumber: &season, DownloadID: testDownloadID,
		Protocol: "torrent", OutputPath: "/downloads/Clockwork Garden", DownloadClient: "qBittorrent",
	}
	manifest := qbittorrent.File{Index: 0, Name: "[03].mkv", Size: 100, Progress: 1, Priority: 1}
	torrent := qbittorrent.Torrent{Hash: testDownloadID, Name: "Clockwork Garden", State: "uploading", Category: "sonarr"}
	digest, err := manifestDigest(torrent, []qbittorrent.File{manifest})
	if err != nil {
		t.Fatal(err)
	}
	sonarrAPI := &safetySonarr{queue: []sonarr.QueueRecord{record}}
	engine := &Engine{
		sonarr:      sonarrAPI,
		qbittorrent: &safetyQBittorrent{torrent: torrent, files: []qbittorrent.File{manifest}},
	}
	built := plan{
		result: Result{QueueFinalization: &QueueFinalization{
			Status: "pending", PendingQueueID: record.ID,
		}},
		context:        mapper.Context{DownloadID: testDownloadID},
		queueRecords:   []sonarr.QueueRecord{record},
		torrent:        torrent,
		manifestSHA256: digest,
	}

	err = engine.finalizeQueue(context.Background(), &built)
	if err == nil || !strings.Contains(err.Error(), "will not be repeated automatically") {
		t.Fatalf("pending DELETE error = %v", err)
	}
	if sonarrAPI.finalizeCalls != 0 {
		t.Fatalf("uncertain queue DELETE was repeated %d times", sonarrAPI.finalizeCalls)
	}
}

func TestRenameIntentIsDurableBeforeMutationAndManifestIsVerified(t *testing.T) {
	store, err := newOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original := qbittorrent.File{Index: 0, Name: "Release/[01].mkv", Size: 100, Progress: 1, Priority: 1}
	target := "Release/Futurama.S01E01.WEBDL-720p.mkv"
	torrent := qbittorrent.Torrent{Hash: testDownloadID, Name: "Release", State: "uploading", Category: "sonarr"}
	digest, err := manifestDigest(torrent, []qbittorrent.File{original})
	if err != nil {
		t.Fatal(err)
	}
	built := plan{
		result: Result{
			Mode: "execute", Outcome: "ready", PlanToken: "sha256:rename",
			Files: []FileResult{{Index: 0, RelativePath: original.Name, Rename: &RenameResult{FromPath: original.Name, ToPath: target, Status: "planned"}}},
		},
		context: mapper.Context{DownloadID: testDownloadID}, torrent: torrent,
		manifest: []qbittorrent.File{original}, manifestSHA256: digest,
		prepared: []preparedFile{{resultIndex: 0, manifest: original, originalPath: original.Name, targetPath: target}},
	}
	qbit := &safetyQBittorrent{torrent: torrent, files: []qbittorrent.File{original}}
	qbit.rename = func(hash, oldPath, newPath string) error {
		record, exists, loadErr := store.load(testDownloadID)
		if loadErr != nil || !exists || record.Phase != "rename_file_submitting" {
			t.Fatalf("rename intent was not durable before API call: exists=%v phase=%q err=%v", exists, record.Phase, loadErr)
		}
		if hash != testDownloadID || oldPath != original.Name || newPath != target {
			t.Fatalf("unexpected rename request: %q %q %q", hash, oldPath, newPath)
		}
		qbit.files[0].Name = target
		return nil
	}
	engine := &Engine{qbittorrent: qbit, operations: store}
	if err := engine.performRenames(context.Background(), &built); err != nil {
		t.Fatal(err)
	}
	if qbit.renameCalls != 1 || built.manifest[0].Name != target || !built.prepared[0].renameApplied || built.result.Files[0].Rename.Status != "applied" {
		t.Fatalf("rename was not fully reconciled: calls=%d built=%+v", qbit.renameCalls, built)
	}
	record, exists, err := store.load(testDownloadID)
	if err != nil || !exists || record.Phase != "renames_verified" {
		t.Fatalf("verified rename phase not persisted: exists=%v phase=%q err=%v", exists, record.Phase, err)
	}
}

func TestRenameRecoveryObservesAppliedTargetWithoutRepeatingMutation(t *testing.T) {
	store, err := newOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original := qbittorrent.File{Index: 0, Name: "Release/[01].mkv", Size: 100, Progress: 1, Priority: 1}
	targetFile := original
	targetFile.Name = "Release/Futurama.S01E01.WEBDL-720p.mkv"
	torrent := qbittorrent.Torrent{Hash: testDownloadID, Name: "Release", State: "stalledUP", Category: "sonarr"}
	digest, err := manifestDigest(torrent, []qbittorrent.File{original})
	if err != nil {
		t.Fatal(err)
	}
	built := plan{
		result:  Result{PlanToken: "sha256:recover", Files: []FileResult{{Rename: &RenameResult{FromPath: original.Name, ToPath: targetFile.Name, Status: "planned"}}}},
		context: mapper.Context{DownloadID: testDownloadID}, torrent: torrent,
		manifest: []qbittorrent.File{original}, manifestSHA256: digest,
		prepared: []preparedFile{{resultIndex: 0, manifest: original, originalPath: original.Name, targetPath: targetFile.Name}},
	}
	qbit := &safetyQBittorrent{torrent: torrent, files: []qbittorrent.File{targetFile}}
	engine := &Engine{qbittorrent: qbit, operations: store}
	if err := engine.performRenames(context.Background(), &built); err != nil {
		t.Fatal(err)
	}
	if qbit.renameCalls != 0 || !built.prepared[0].renameApplied {
		t.Fatalf("recovery repeated a proven rename: calls=%d applied=%v", qbit.renameCalls, built.prepared[0].renameApplied)
	}
}

func TestRenameConflictBecomesDurableBlocker(t *testing.T) {
	store, err := newOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original := qbittorrent.File{Index: 0, Name: "Release/[01].mkv", Size: 100, Progress: 1, Priority: 1}
	target := "Release/Futurama.S01E01.WEBDL-720p.mkv"
	torrent := qbittorrent.Torrent{Hash: testDownloadID, Name: "Release", State: "stalledUP", Category: "sonarr"}
	digest, err := manifestDigest(torrent, []qbittorrent.File{original})
	if err != nil {
		t.Fatal(err)
	}
	built := plan{
		result:  Result{PlanToken: "sha256:conflict", Files: []FileResult{{Rename: &RenameResult{FromPath: original.Name, ToPath: target, Status: "planned"}}}},
		context: mapper.Context{DownloadID: testDownloadID}, torrent: torrent,
		manifest: []qbittorrent.File{original}, manifestSHA256: digest,
		prepared: []preparedFile{{resultIndex: 0, manifest: original, originalPath: original.Name, targetPath: target}},
	}
	qbit := &safetyQBittorrent{torrent: torrent, files: []qbittorrent.File{original}}
	qbit.rename = func(string, string, string) error {
		return &qbittorrent.APIError{Method: "POST", Endpoint: "/api/v2/torrents/renameFile", StatusCode: 409, Message: "target in use"}
	}
	engine := &Engine{qbittorrent: qbit, operations: store, commandTimeout: time.Second, pollInterval: time.Millisecond}
	result, err := engine.runPreparedOperation(context.Background(), &built)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "blocked" || result.OperationPhase != "rename_blocked" || len(result.Blockers) != 1 || result.Blockers[0].Code != "QBITTORRENT_RENAME_REJECTED" {
		t.Fatalf("rename conflict was not persisted as a blocker: %+v", result)
	}
	record, exists, err := store.load(testDownloadID)
	if err != nil || !exists || record.Phase != "rename_blocked" {
		t.Fatalf("rename blocker record mismatch: exists=%v phase=%q err=%v", exists, record.Phase, err)
	}
}

func TestPartialRenameConflictResumesPersistedPlanWithSameToken(t *testing.T) {
	store, err := newOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seriesID, season := 101, 1
	originals := []qbittorrent.File{
		{Index: 0, Name: "Release/[01].mkv", Size: 100, Progress: 1, Priority: 1},
		{Index: 1, Name: "Release/[02].mkv", Size: 200, Progress: 1, Priority: 1},
	}
	targets := []string{"Release/Futurama.S01E01.WEBDL-720p.mkv", "Release/Futurama.S01E02.WEBDL-720p.mkv"}
	torrent := qbittorrent.Torrent{
		Hash: testDownloadID, Name: "Release", State: "stalledUP", Category: "sonarr",
		SavePath: "/downloads/", ContentPath: "/downloads/Release",
	}
	digest, err := manifestDigest(torrent, originals)
	if err != nil {
		t.Fatal(err)
	}
	queueRecord := sonarr.QueueRecord{
		ID: 11, SeriesID: &seriesID, SeasonNumber: &season, DownloadID: testDownloadID,
		Protocol: "torrent", DownloadClient: "qBittorrent", OutputPath: "/downloads/Release",
	}
	built := plan{
		result: Result{
			Mode: "execute", Outcome: "ready", CanExecute: true, PlanToken: "sha256:partial-rename",
			Files: []FileResult{
				{Index: 0, RelativePath: originals[0].Name, Size: originals[0].Size, Outcome: "ready", Mapping: &mapper.Decision{EpisodeIDs: []int{1101}}, Rename: &RenameResult{FromPath: originals[0].Name, ToPath: targets[0], Status: "planned"}},
				{Index: 1, RelativePath: originals[1].Name, Size: originals[1].Size, Outcome: "ready", Mapping: &mapper.Decision{EpisodeIDs: []int{1102}}, Rename: &RenameResult{FromPath: originals[1].Name, ToPath: targets[1], Status: "planned"}},
			},
		},
		selection:  Selection{DownloadID: testDownloadID},
		context:    mapper.Context{SeriesID: seriesID, SeasonNumber: season, QueueIDs: []int{queueRecord.ID}, DownloadID: testDownloadID, Source: "sonarrQueue"},
		outputPath: queueRecord.OutputPath, queueRecords: []sonarr.QueueRecord{queueRecord},
		torrent: torrent, manifest: append([]qbittorrent.File(nil), originals...), manifestSHA256: digest,
		historyBaseline: map[int]struct{}{},
		prepared: []preparedFile{
			{resultIndex: 0, manifest: originals[0], originalPath: originals[0].Name, targetPath: targets[0], expectedSourcePath: "/downloads/Release/Futurama.S01E01.WEBDL-720p.mkv", candidate: sonarr.ManualImportCandidate{DownloadID: testDownloadID}},
			{resultIndex: 1, manifest: originals[1], originalPath: originals[1].Name, targetPath: targets[1], expectedSourcePath: "/downloads/Release/Futurama.S01E02.WEBDL-720p.mkv", candidate: sonarr.ManualImportCandidate{DownloadID: testDownloadID}},
		},
	}

	remediated := false
	qbit := &safetyQBittorrent{torrent: torrent, files: append([]qbittorrent.File(nil), originals...)}
	qbit.rename = func(_ string, oldPath, newPath string) error {
		switch oldPath {
		case originals[0].Name:
			qbit.files[0].Name = newPath
			return nil
		case originals[1].Name:
			if !remediated {
				return &qbittorrent.APIError{Method: "POST", Endpoint: "/api/v2/torrents/renameFile", StatusCode: 409, Message: "target in use"}
			}
			qbit.files[1].Name = newPath
			return nil
		default:
			t.Fatalf("unexpected repeated rename from %q", oldPath)
			return nil
		}
	}
	sonarrAPI := &safetySonarr{
		episodes: []sonarr.Episode{
			{ID: 1101, SeriesID: seriesID, SeasonNumber: season, HasFile: true, EpisodeFileID: 701},
			{ID: 1102, SeriesID: seriesID, SeasonNumber: season, HasFile: true, EpisodeFileID: 702},
		},
		episodeFiles: []sonarr.EpisodeFile{
			{ID: 701, SeriesID: seriesID, SeasonNumber: season, Path: "/library/Futurama/S01E01.mkv", Size: 100},
			{ID: 702, SeriesID: seriesID, SeasonNumber: season, Path: "/library/Futurama/S01E02.mkv", Size: 200},
		},
		history: []sonarr.HistoryRecord{
			{ID: 901, EpisodeID: 1101, SeriesID: seriesID, DownloadID: testDownloadID, EventType: "downloadFolderImported", Data: map[string]string{"droppedPath": built.prepared[0].expectedSourcePath, "fileId": "701", "importedPath": "/library/Futurama/S01E01.mkv"}},
			{ID: 902, EpisodeID: 1102, SeriesID: seriesID, DownloadID: testDownloadID, EventType: "downloadFolderImported", Data: map[string]string{"droppedPath": built.prepared[1].expectedSourcePath, "fileId": "702", "importedPath": "/library/Futurama/S01E02.mkv"}},
		},
	}
	engine := &Engine{
		sonarr: sonarrAPI, qbittorrent: qbit, operations: store,
		commandTimeout: time.Second, workflowTimeout: 3 * time.Second, pollInterval: time.Millisecond,
	}

	blockedResult, err := engine.runPreparedOperation(context.Background(), &built)
	if err != nil {
		t.Fatal(err)
	}
	if blockedResult.OperationPhase != "rename_blocked" || qbit.renameCalls != 2 {
		t.Fatalf("partial conflict was not durably blocked: calls=%d result=%+v", qbit.renameCalls, blockedResult)
	}
	remediated = true
	result, err := engine.Run(context.Background(), Selection{DownloadID: testDownloadID}, true, built.result.PlanToken)
	if err != nil {
		t.Fatal(err)
	}
	if qbit.renameCalls != 3 || qbit.files[0].Name != targets[0] || qbit.files[1].Name != targets[1] {
		t.Fatalf("persisted partial plan was not resumed exactly once: calls=%d files=%+v", qbit.renameCalls, qbit.files)
	}
	if result.Outcome != "imported" || result.OperationPhase != "complete" {
		t.Fatalf("resumed operation did not complete: %+v", result)
	}
}

func TestEarlyExpectedCategoryTransitionDoesNotBlockRemainingRenames(t *testing.T) {
	store, err := newOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seriesID, season := 101, 1
	originals := []qbittorrent.File{
		{Index: 0, Name: "Release/[01].mkv", Size: 100, Progress: 1, Priority: 1},
		{Index: 1, Name: "Release/[02].mkv", Size: 200, Progress: 1, Priority: 1},
	}
	targets := []string{"Release/Futurama.S01E01.WEBDL-720p.mkv", "Release/Futurama.S01E02.WEBDL-720p.mkv"}
	torrent := qbittorrent.Torrent{Hash: testDownloadID, Name: "Release", State: "stalledUP", Category: "sonarr"}
	digest, err := manifestDigest(torrent, originals)
	if err != nil {
		t.Fatal(err)
	}
	built := plan{
		result: Result{PlanToken: "sha256:category", Files: []FileResult{
			{Rename: &RenameResult{FromPath: originals[0].Name, ToPath: targets[0], Status: "planned"}},
			{Rename: &RenameResult{FromPath: originals[1].Name, ToPath: targets[1], Status: "planned"}},
		}},
		context:      mapper.Context{DownloadID: testDownloadID},
		queueRecords: []sonarr.QueueRecord{{ID: 11, SeriesID: &seriesID, SeasonNumber: &season, DownloadClientHasPostImportCategory: true}},
		torrent:      torrent, manifest: append([]qbittorrent.File(nil), originals...), manifestSHA256: digest,
		prepared: []preparedFile{
			{resultIndex: 0, manifest: originals[0], originalPath: originals[0].Name, targetPath: targets[0]},
			{resultIndex: 1, manifest: originals[1], originalPath: originals[1].Name, targetPath: targets[1]},
		},
	}
	qbit := &safetyQBittorrent{torrent: torrent, files: append([]qbittorrent.File(nil), originals...)}
	qbit.rename = func(_ string, oldPath, newPath string) error {
		for index := range qbit.files {
			if qbit.files[index].Name == oldPath {
				qbit.files[index].Name = newPath
				if index == 0 {
					qbit.torrent.Category = "sonarr-imported"
				}
				return nil
			}
		}
		return errors.New("source path not found")
	}
	engine := &Engine{qbittorrent: qbit, operations: store, commandTimeout: time.Second, pollInterval: time.Millisecond}
	if err := engine.performRenames(context.Background(), &built); err != nil {
		t.Fatal(err)
	}
	if qbit.renameCalls != 2 || built.observedCategory != "sonarr-imported" || qbit.files[1].Name != targets[1] {
		t.Fatalf("expected early category transition blocked the batch: calls=%d category=%q files=%+v", qbit.renameCalls, built.observedCategory, qbit.files)
	}
}

func TestStalePersistedImportedOutcomeNeverFinalizesWithoutCurrentEvidence(t *testing.T) {
	store, err := newOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seriesID, season, episodeID := 101, 1, 1101
	manifest := qbittorrent.File{Index: 0, Name: "Release/Futurama.S01E01.WEBDL-720p.mkv", Size: 100, Progress: 1, Priority: 1}
	torrent := qbittorrent.Torrent{Hash: testDownloadID, Name: "Release", State: "stalledUP", Category: "sonarr"}
	digest, err := manifestDigest(torrent, []qbittorrent.File{manifest})
	if err != nil {
		t.Fatal(err)
	}
	built := plan{
		result: Result{Mode: "execute", Outcome: "ready", CanExecute: true, PlanToken: "sha256:stale-imported", Files: []FileResult{{
			Index: 0, RelativePath: manifest.Name, Size: manifest.Size, Outcome: "imported", Mapping: &mapper.Decision{EpisodeIDs: []int{episodeID}},
			Verification: &Verification{HistoryID: 900, EpisodeID: episodeID, EpisodeFileID: 700},
		}}},
		selection: Selection{DownloadID: testDownloadID}, context: mapper.Context{SeriesID: seriesID, SeasonNumber: season, DownloadID: testDownloadID},
		outputPath: "/downloads/Release", torrent: torrent, manifest: []qbittorrent.File{manifest}, manifestSHA256: digest,
		historyBaseline: map[int]struct{}{},
		prepared: []preparedFile{{
			resultIndex: 0, manifest: manifest, originalPath: manifest.Name, targetPath: manifest.Name,
			expectedSourcePath: "/downloads/Release/Futurama.S01E01.WEBDL-720p.mkv", renameApplied: true,
			candidate: sonarr.ManualImportCandidate{DownloadID: testDownloadID},
		}},
	}
	if err := store.save(testDownloadID, operationRecord{PlanToken: built.result.PlanToken, Phase: "manual_import_ready", Plan: persistPlan(built)}); err != nil {
		t.Fatal(err)
	}
	sonarrAPI := &safetySonarr{episodes: []sonarr.Episode{{ID: episodeID, SeriesID: seriesID, SeasonNumber: season}}}
	engine := &Engine{
		sonarr: sonarrAPI, qbittorrent: &safetyQBittorrent{torrent: torrent, files: []qbittorrent.File{manifest}}, operations: store,
		commandTimeout: time.Millisecond, workflowTimeout: time.Second, pollInterval: time.Millisecond,
	}
	result, err := engine.Run(context.Background(), Selection{DownloadID: testDownloadID}, true, built.result.PlanToken)
	if err == nil {
		t.Fatalf("stale persisted import evidence unexpectedly completed: %+v", result)
	}
	if sonarrAPI.startCalls != 0 || sonarrAPI.finalizeCalls != 0 || result.OperationPhase == "complete" || result.OperationPhase == "import_verified" {
		t.Fatalf("stale evidence triggered a mutation or finalization: starts=%d finalizes=%d result=%+v", sonarrAPI.startCalls, sonarrAPI.finalizeCalls, result)
	}
}

func TestManualImportPreflightRejectsLateStorageTopologyChange(t *testing.T) {
	store, err := newOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seriesID, season, episodeID := 101, 1, 1101
	manifest := qbittorrent.File{Index: 0, Name: "Release/Futurama.S01E01.WEBDL-720p.mkv", Size: 100, Progress: 1, Priority: 1}
	torrent := qbittorrent.Torrent{Hash: testDownloadID, Name: "Release", State: "stalledUP", Category: "sonarr", SavePath: "/downloads/", ContentPath: "/downloads/Release"}
	digest, err := manifestDigest(torrent, []qbittorrent.File{manifest})
	if err != nil {
		t.Fatal(err)
	}
	expectedPath := "/downloads/Release/Futurama.S01E01.WEBDL-720p.mkv"
	built := plan{
		result: Result{Mode: "execute", Outcome: "ready", CanExecute: true, PlanToken: "sha256:late-move", Files: []FileResult{{
			Index: 0, RelativePath: manifest.Name, Size: manifest.Size, Outcome: "ready", Mapping: &mapper.Decision{EpisodeIDs: []int{episodeID}},
		}}},
		selection: Selection{DownloadID: testDownloadID}, context: mapper.Context{SeriesID: seriesID, SeasonNumber: season, DownloadID: testDownloadID},
		torrent: torrent, manifest: []qbittorrent.File{manifest}, manifestSHA256: digest, historyBaseline: map[int]struct{}{},
		prepared: []preparedFile{{
			resultIndex: 0, manifest: manifest, originalPath: manifest.Name, targetPath: manifest.Name, expectedSourcePath: expectedPath, renameApplied: true,
			candidate: sonarr.ManualImportCandidate{Path: expectedPath, DownloadID: testDownloadID}, commandFile: sonarr.ManualImportFile{Path: expectedPath, DownloadID: testDownloadID},
		}},
	}
	qbit := &safetyQBittorrent{torrent: torrent, files: []qbittorrent.File{manifest}}
	qbit.torrentFn = func(call int) qbittorrent.Torrent {
		current := torrent
		if call >= 2 {
			current.SavePath = "/moved/"
			current.ContentPath = "/moved/Release"
		}
		return current
	}
	sonarrAPI := &safetySonarr{episodes: []sonarr.Episode{{ID: episodeID, SeriesID: seriesID, SeasonNumber: season}}}
	engine := &Engine{
		sonarr: sonarrAPI, qbittorrent: qbit, operations: store,
		commandTimeout: time.Second, workflowTimeout: 2 * time.Second, pollInterval: time.Millisecond,
	}
	result, err := engine.runPreparedOperation(context.Background(), &built)
	if err == nil || !strings.Contains(err.Error(), "storage topology") {
		t.Fatalf("late storage move was not rejected before ManualImport: result=%+v err=%v", result, err)
	}
	if sonarrAPI.startCalls != 0 {
		t.Fatalf("ManualImport was submitted after storage moved: %d", sonarrAPI.startCalls)
	}
}

func TestTransientCandidateFailureKeepsOriginalOutputPathRetryable(t *testing.T) {
	store, err := newOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seriesID, season, episodeID, episodeFileID := 101, 1, 1101, 700
	originalPath := "Release/[01].mkv"
	targetPath := "Release/Futurama.S01E01.WEBDL-720p.mkv"
	expectedPath := "/downloads/Release/Futurama.S01E01.WEBDL-720p.mkv"
	originalOutputPath := "/downloads/Release/[01].mkv"
	manifest := qbittorrent.File{Index: 0, Name: targetPath, Size: 100, Progress: 1, Priority: 1}
	torrent := qbittorrent.Torrent{Hash: testDownloadID, Name: "Release", State: "stalledUP", Category: "sonarr"}
	digest, err := manifestDigest(torrent, []qbittorrent.File{manifest})
	if err != nil {
		t.Fatal(err)
	}
	built := plan{
		result: Result{Mode: "execute", Outcome: "ready", CanExecute: true, PlanToken: "sha256:candidate-retry", Files: []FileResult{{
			Index: 0, RelativePath: originalPath, Size: manifest.Size, Outcome: "ready", Mapping: &mapper.Decision{EpisodeIDs: []int{episodeID}},
			Rename: &RenameResult{FromPath: originalPath, ToPath: targetPath, Status: "applied"},
		}}},
		selection: Selection{DownloadID: testDownloadID}, context: mapper.Context{SeriesID: seriesID, SeasonNumber: season, DownloadID: testDownloadID},
		outputPath: originalOutputPath, torrent: torrent, manifest: []qbittorrent.File{manifest}, manifestSHA256: digest, historyBaseline: map[int]struct{}{},
		prepared: []preparedFile{{
			resultIndex: 0, manifest: manifest, originalPath: originalPath, targetPath: targetPath, expectedSourcePath: expectedPath, renameApplied: true,
			candidate: sonarr.ManualImportCandidate{DownloadID: testDownloadID},
		}},
	}
	quality := json.RawMessage(`{"quality":{"name":"WEBDL-720p"}}`)
	languages := json.RawMessage(`[{"id":1,"name":"English"}]`)
	releaseType := json.RawMessage(`"singleEpisode"`)
	seasonCopy := season
	sonarrAPI := &safetySonarr{episodes: []sonarr.Episode{{ID: episodeID, SeriesID: seriesID, SeasonNumber: season}}}
	sonarrAPI.candidates = func(call int, path, downloadID string) ([]sonarr.ManualImportCandidate, error) {
		if path != expectedPath || downloadID != testDownloadID {
			t.Fatalf("candidate lookup path changed across retry: path=%q downloadID=%q", path, downloadID)
		}
		if call == 1 {
			return nil, errors.New("transient Sonarr error")
		}
		sonarrAPI.episodes = []sonarr.Episode{{ID: episodeID, SeriesID: seriesID, SeasonNumber: season, HasFile: true, EpisodeFileID: episodeFileID}}
		sonarrAPI.episodeFiles = []sonarr.EpisodeFile{{ID: episodeFileID, SeriesID: seriesID, SeasonNumber: season, Path: "/library/Futurama/S01E01.mkv", Size: manifest.Size}}
		sonarrAPI.history = []sonarr.HistoryRecord{{
			ID: 900, EpisodeID: episodeID, SeriesID: seriesID, DownloadID: testDownloadID, EventType: "downloadFolderImported",
			Data: map[string]string{"droppedPath": expectedPath, "fileId": "700", "importedPath": "/library/Futurama/S01E01.mkv"},
		}}
		return []sonarr.ManualImportCandidate{{
			Path: expectedPath, RelativePath: pathBase(targetPath), Size: manifest.Size,
			Series: &sonarr.SeriesRef{ID: seriesID}, SeasonNumber: &seasonCopy, DownloadID: testDownloadID,
			Quality: quality, Languages: languages, ReleaseType: releaseType,
		}}, nil
	}
	sonarrAPI.reprocess = func(requests []sonarr.ManualImportReprocess) ([]sonarr.ManualImportReprocess, error) {
		if len(requests) != 1 {
			t.Fatalf("unexpected reprocess request count: %d", len(requests))
		}
		return []sonarr.ManualImportReprocess{{
			Path: expectedPath, SeriesID: seriesID, SeasonNumber: &seasonCopy,
			Episodes: []sonarr.Episode{{ID: episodeID, SeriesID: seriesID, SeasonNumber: season}},
			Quality:  quality, Languages: languages, ReleaseType: releaseType, DownloadID: testDownloadID,
		}}, nil
	}
	engine := &Engine{
		sonarr: sonarrAPI, qbittorrent: &safetyQBittorrent{torrent: torrent, files: []qbittorrent.File{manifest}}, operations: store,
		commandTimeout: time.Millisecond, workflowTimeout: time.Second, pollInterval: time.Millisecond,
	}

	first, err := engine.runPreparedOperation(context.Background(), &built)
	if err == nil || !strings.Contains(err.Error(), "transient Sonarr error") {
		t.Fatalf("first candidate failure was not surfaced: result=%+v err=%v", first, err)
	}
	record, exists, loadErr := store.load(testDownloadID)
	if loadErr != nil || !exists || record.Plan.OutputPath != originalOutputPath {
		t.Fatalf("fallible candidate discovery mutated durable outputPath: exists=%v path=%q err=%v", exists, record.Plan.OutputPath, loadErr)
	}
	result, err := engine.Run(context.Background(), Selection{DownloadID: testDownloadID}, true, built.result.PlanToken)
	if err != nil {
		t.Fatal(err)
	}
	if sonarrAPI.candidateCalls != 2 || result.Outcome != "imported" || result.OperationPhase != "complete" {
		t.Fatalf("same-token retry did not recover candidate discovery: calls=%d result=%+v", sonarrAPI.candidateCalls, result)
	}
}

func TestLateRecoveryPhasesRevalidatePersistedImportEvidence(t *testing.T) {
	for _, phase := range []string{"import_verified", "queue_finalizing"} {
		t.Run(phase, func(t *testing.T) {
			store, err := newOperationStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			seriesID, season, episodeID := 101, 1, 1101
			manifest := qbittorrent.File{Index: 0, Name: "Release/Futurama.S01E01.WEBDL-720p.mkv", Size: 100, Progress: 1, Priority: 1}
			torrent := qbittorrent.Torrent{Hash: testDownloadID, Name: "Release", State: "stalledUP", Category: "sonarr"}
			digest, err := manifestDigest(torrent, []qbittorrent.File{manifest})
			if err != nil {
				t.Fatal(err)
			}
			queueRecord := sonarr.QueueRecord{ID: 11, SeriesID: &seriesID, SeasonNumber: &season, DownloadID: testDownloadID, Protocol: "torrent", DownloadClient: "qBittorrent"}
			expectedPath := "/downloads/Release/Futurama.S01E01.WEBDL-720p.mkv"
			built := plan{
				result: Result{Mode: "execute", Outcome: "imported", PlanToken: "sha256:late-evidence-" + phase, OperationPhase: phase, Files: []FileResult{{
					Index: 0, RelativePath: manifest.Name, Size: manifest.Size, Outcome: "imported", Mapping: &mapper.Decision{EpisodeIDs: []int{episodeID}},
					Verification: &Verification{HistoryID: 900, EpisodeID: episodeID, EpisodeFileID: 700},
				}}},
				selection: Selection{DownloadID: testDownloadID}, context: mapper.Context{SeriesID: seriesID, SeasonNumber: season, QueueIDs: []int{queueRecord.ID}, DownloadID: testDownloadID},
				queueRecords: []sonarr.QueueRecord{queueRecord}, torrent: torrent, manifest: []qbittorrent.File{manifest}, manifestSHA256: digest,
				historyBaseline: map[int]struct{}{},
				prepared: []preparedFile{{
					resultIndex: 0, manifest: manifest, originalPath: manifest.Name, targetPath: manifest.Name, expectedSourcePath: expectedPath, renameApplied: true,
					candidate: sonarr.ManualImportCandidate{DownloadID: testDownloadID},
				}},
			}
			if err := store.save(testDownloadID, operationRecord{PlanToken: built.result.PlanToken, Phase: phase, Plan: persistPlan(built)}); err != nil {
				t.Fatal(err)
			}
			sonarrAPI := &safetySonarr{queue: []sonarr.QueueRecord{queueRecord}, episodes: []sonarr.Episode{{ID: episodeID, SeriesID: seriesID, SeasonNumber: season}}}
			engine := &Engine{
				sonarr: sonarrAPI, qbittorrent: &safetyQBittorrent{torrent: torrent, files: []qbittorrent.File{manifest}}, operations: store,
				commandTimeout: time.Millisecond, workflowTimeout: time.Second, pollInterval: time.Millisecond,
			}
			result, err := engine.Run(context.Background(), Selection{DownloadID: testDownloadID}, true, built.result.PlanToken)
			if err == nil {
				t.Fatalf("phase %s completed with stale import evidence: %+v", phase, result)
			}
			if sonarrAPI.finalizeCalls != 0 || result.OperationPhase != phase {
				t.Fatalf("phase %s mutated queue or lost recovery state: finalizes=%d result=%+v", phase, sonarrAPI.finalizeCalls, result)
			}
		})
	}
}

func TestEpisodeHasFileWithoutExactHistoryBlocksManualImportSubmission(t *testing.T) {
	store, err := newOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seriesID, season, episodeID, episodeFileID := 101, 1, 1101, 700
	manifest := qbittorrent.File{Index: 0, Name: "Release/Futurama.S01E01.WEBDL-720p.mkv", Size: 100, Progress: 1, Priority: 1}
	torrent := qbittorrent.Torrent{Hash: testDownloadID, Name: "Release", State: "stalledUP", Category: "sonarr"}
	digest, err := manifestDigest(torrent, []qbittorrent.File{manifest})
	if err != nil {
		t.Fatal(err)
	}
	expectedPath := "/downloads/Release/Futurama.S01E01.WEBDL-720p.mkv"
	built := plan{
		result: Result{Mode: "execute", Outcome: "ready", CanExecute: true, PlanToken: "sha256:history-race", Files: []FileResult{{
			Index: 0, RelativePath: manifest.Name, Size: manifest.Size, Outcome: "ready", Mapping: &mapper.Decision{EpisodeIDs: []int{episodeID}},
		}}},
		selection: Selection{DownloadID: testDownloadID}, context: mapper.Context{SeriesID: seriesID, SeasonNumber: season, DownloadID: testDownloadID},
		torrent: torrent, manifest: []qbittorrent.File{manifest}, manifestSHA256: digest, historyBaseline: map[int]struct{}{},
		prepared: []preparedFile{{
			resultIndex: 0, manifest: manifest, originalPath: manifest.Name, targetPath: manifest.Name, expectedSourcePath: expectedPath, renameApplied: true,
			candidate: sonarr.ManualImportCandidate{Path: expectedPath, DownloadID: testDownloadID}, commandFile: sonarr.ManualImportFile{Path: expectedPath, DownloadID: testDownloadID},
		}},
	}
	sonarrAPI := &safetySonarr{
		episodes:     []sonarr.Episode{{ID: episodeID, SeriesID: seriesID, SeasonNumber: season, HasFile: true, EpisodeFileID: episodeFileID}},
		episodeFiles: []sonarr.EpisodeFile{{ID: episodeFileID, SeriesID: seriesID, SeasonNumber: season, Path: "/library/Futurama/S01E01.mkv", Size: manifest.Size}},
	}
	engine := &Engine{
		sonarr: sonarrAPI, qbittorrent: &safetyQBittorrent{torrent: torrent, files: []qbittorrent.File{manifest}}, operations: store,
		commandTimeout: time.Millisecond, workflowTimeout: time.Second, pollInterval: time.Millisecond,
	}
	result, err := engine.runPreparedOperation(context.Background(), &built)
	if err == nil || !strings.Contains(err.Error(), "without exact current import evidence") {
		t.Fatalf("history/episode race was not blocked: result=%+v err=%v", result, err)
	}
	if sonarrAPI.startCalls != 0 {
		t.Fatalf("ManualImport was submitted while episode already had an unverified file: %d", sonarrAPI.startCalls)
	}
}
