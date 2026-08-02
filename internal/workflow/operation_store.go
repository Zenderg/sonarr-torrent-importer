package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/zenderg/sonarr-torrent-importer/internal/mapper"
	"github.com/zenderg/sonarr-torrent-importer/internal/qbittorrent"
	"github.com/zenderg/sonarr-torrent-importer/internal/sonarr"
)

const operationRecordVersion = 2

var errExecutionLocked = errors.New("another execute workflow holds the persistent operation lock")

type operationStore struct {
	directory  string
	lockPath   string
	latestPath string
}

type operationLock struct {
	file *os.File
}

type operationRecord struct {
	Version   int           `json:"version"`
	PlanToken string        `json:"planToken"`
	Phase     string        `json:"phase"`
	CommandID int           `json:"commandId,omitempty"`
	UpdatedAt time.Time     `json:"updatedAt"`
	Plan      persistedPlan `json:"plan"`
}

type persistedPlan struct {
	Result           Result               `json:"result"`
	Selection        Selection            `json:"selection"`
	Context          mapper.Context       `json:"context"`
	OutputPath       string               `json:"outputPath"`
	QueueRecords     []sonarr.QueueRecord `json:"queueRecords"`
	Episodes         []sonarr.Episode     `json:"episodes"`
	Torrent          qbittorrent.Torrent  `json:"torrent"`
	Manifest         []qbittorrent.File   `json:"manifest"`
	ManifestSHA256   string               `json:"manifestSha256"`
	ObservedCategory string               `json:"observedCategory,omitempty"`
	HistoryBaseline  []int                `json:"historyBaseline"`
	Prepared         []persistedPrepared  `json:"prepared"`
}

type persistedPrepared struct {
	ResultIndex        int                          `json:"resultIndex"`
	Manifest           qbittorrent.File             `json:"manifest"`
	OriginalPath       string                       `json:"originalPath"`
	TargetPath         string                       `json:"targetPath"`
	ExpectedSourcePath string                       `json:"expectedSourcePath"`
	RenameApplied      bool                         `json:"renameApplied"`
	Candidate          sonarr.ManualImportCandidate `json:"candidate"`
	CommandFile        sonarr.ManualImportFile      `json:"commandFile"`
}

func newOperationStore(dataRoot string) (*operationStore, error) {
	directory := filepath.Join(dataRoot, "operations")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create operation state directory: %w", err)
	}
	root, err := os.Open(dataRoot)
	if err != nil {
		return nil, fmt.Errorf("open operation state root for sync: %w", err)
	}
	if err := root.Sync(); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("sync operation state root: %w", err)
	}
	if err := root.Close(); err != nil {
		return nil, fmt.Errorf("close operation state root: %w", err)
	}
	return &operationStore{
		directory:  directory,
		lockPath:   filepath.Join(dataRoot, "execute.lock"),
		latestPath: filepath.Join(directory, "latest.json"),
	}, nil
}

func (s *operationStore) tryLock() (*operationLock, error) {
	file, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open operation lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errExecutionLocked
		}
		return nil, fmt.Errorf("acquire operation lock: %w", err)
	}
	return &operationLock{file: file}, nil
}

func (l *operationLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func (s *operationStore) load(downloadID string) (operationRecord, bool, error) {
	return s.loadPath(s.recordPath(downloadID))
}

func (s *operationStore) loadLatest() (operationRecord, bool, error) {
	return s.loadPath(s.latestPath)
}

func (s *operationStore) loadPath(path string) (operationRecord, bool, error) {
	encoded, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return operationRecord{}, false, nil
	}
	if err != nil {
		return operationRecord{}, false, fmt.Errorf("read operation state: %w", err)
	}
	var record operationRecord
	if err := json.Unmarshal(encoded, &record); err != nil {
		return operationRecord{}, false, fmt.Errorf("decode operation state: %w", err)
	}
	if record.Version != operationRecordVersion || record.PlanToken == "" || record.Phase == "" {
		return operationRecord{}, false, fmt.Errorf("operation state has an unsupported or incomplete format")
	}
	return record, true, nil
}

func (s *operationStore) save(downloadID string, record operationRecord) error {
	record.Version = operationRecordVersion
	record.UpdatedAt = time.Now().UTC()
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode operation state: %w", err)
	}
	encoded = append(encoded, '\n')

	if err := s.writeAtomic(s.recordPath(downloadID), encoded); err != nil {
		return err
	}
	if err := s.writeAtomic(s.latestPath, encoded); err != nil {
		return fmt.Errorf("update latest operation state: %w", err)
	}
	directory, err := os.Open(s.directory)
	if err != nil {
		return fmt.Errorf("open operation state directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync operation state directory: %w", err)
	}
	return nil
}

func (s *operationStore) writeAtomic(target string, encoded []byte) error {
	temporary, err := os.CreateTemp(s.directory, ".operation-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary operation state: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("protect temporary operation state: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		cleanup()
		return fmt.Errorf("write temporary operation state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temporary operation state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("close temporary operation state: %w", err)
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("replace operation state: %w", err)
	}
	return nil
}

func (s *operationStore) recordPath(downloadID string) string {
	return filepath.Join(s.directory, strings.ToLower(downloadID)+".json")
}

func persistPlan(built plan) persistedPlan {
	baseline := make([]int, 0, len(built.historyBaseline))
	for id := range built.historyBaseline {
		baseline = append(baseline, id)
	}
	sort.Ints(baseline)
	prepared := make([]persistedPrepared, 0, len(built.prepared))
	for _, file := range built.prepared {
		prepared = append(prepared, persistedPrepared{
			ResultIndex:        file.resultIndex,
			Manifest:           file.manifest,
			OriginalPath:       file.originalPath,
			TargetPath:         file.targetPath,
			ExpectedSourcePath: file.expectedSourcePath,
			RenameApplied:      file.renameApplied,
			Candidate:          file.candidate,
			CommandFile:        file.commandFile,
		})
	}
	return persistedPlan{
		Result:           built.result,
		Selection:        built.selection,
		Context:          built.context,
		OutputPath:       built.outputPath,
		QueueRecords:     append([]sonarr.QueueRecord(nil), built.queueRecords...),
		Episodes:         append([]sonarr.Episode(nil), built.episodes...),
		Torrent:          built.torrent,
		Manifest:         append([]qbittorrent.File(nil), built.manifest...),
		ManifestSHA256:   built.manifestSHA256,
		ObservedCategory: built.observedCategory,
		HistoryBaseline:  baseline,
		Prepared:         prepared,
	}
}

func restorePlan(saved persistedPlan) plan {
	baseline := make(map[int]struct{}, len(saved.HistoryBaseline))
	for _, id := range saved.HistoryBaseline {
		baseline[id] = struct{}{}
	}
	prepared := make([]preparedFile, 0, len(saved.Prepared))
	for _, file := range saved.Prepared {
		prepared = append(prepared, preparedFile{
			resultIndex:        file.ResultIndex,
			manifest:           file.Manifest,
			originalPath:       file.OriginalPath,
			targetPath:         file.TargetPath,
			expectedSourcePath: file.ExpectedSourcePath,
			renameApplied:      file.RenameApplied,
			candidate:          file.Candidate,
			commandFile:        file.CommandFile,
		})
	}
	return plan{
		result:           saved.Result,
		selection:        saved.Selection,
		context:          saved.Context,
		outputPath:       saved.OutputPath,
		queueRecords:     append([]sonarr.QueueRecord(nil), saved.QueueRecords...),
		episodes:         append([]sonarr.Episode(nil), saved.Episodes...),
		torrent:          saved.Torrent,
		manifest:         append([]qbittorrent.File(nil), saved.Manifest...),
		manifestSHA256:   saved.ManifestSHA256,
		observedCategory: saved.ObservedCategory,
		historyBaseline:  baseline,
		prepared:         prepared,
	}
}
