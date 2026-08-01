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
	queue         []sonarr.QueueRecord
	episodes      []sonarr.Episode
	history       []sonarr.HistoryRecord
	episodeFiles  []sonarr.EpisodeFile
	startCalls    int
	finalizeCalls int
	finalize      func(int, bool) error
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
func (s *safetySonarr) ManualImportCandidates(context.Context, string, string) ([]sonarr.ManualImportCandidate, error) {
	return nil, errors.New("unexpected ManualImportCandidates call")
}
func (s *safetySonarr) Reprocess(context.Context, []sonarr.ManualImportReprocess) ([]sonarr.ManualImportReprocess, error) {
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
	torrent qbittorrent.Torrent
	files   []qbittorrent.File
}

func (q *safetyQBittorrent) Login(context.Context) error { return nil }
func (q *safetyQBittorrent) Versions(context.Context) (qbittorrent.Versions, error) {
	return qbittorrent.Versions{Application: "5.0", WebAPI: "2.8.2"}, nil
}
func (q *safetyQBittorrent) Torrent(context.Context, string) (qbittorrent.Torrent, error) {
	return q.torrent, nil
}
func (q *safetyQBittorrent) Files(context.Context, string) ([]qbittorrent.File, error) {
	return append([]qbittorrent.File(nil), q.files...), nil
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
		prepared:        []preparedFile{{resultIndex: 0, manifest: manifest, candidate: candidate}},
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
