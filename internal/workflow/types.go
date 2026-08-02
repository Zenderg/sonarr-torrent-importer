package workflow

import (
	"github.com/zenderg/sonarr-torrent-importer/internal/mapper"
	"github.com/zenderg/sonarr-torrent-importer/internal/qbittorrent"
	"github.com/zenderg/sonarr-torrent-importer/internal/sonarr"
)

type Selection struct {
	DownloadID string `json:"downloadId,omitempty"`
	QueueID    int    `json:"queueId,omitempty"`
}

type Result struct {
	Mode              string             `json:"mode"`
	Outcome           string             `json:"outcome"`
	CanExecute        bool               `json:"canExecute"`
	PlanToken         string             `json:"planToken,omitempty"`
	OperationPhase    string             `json:"operationPhase,omitempty"`
	Selection         Selection          `json:"selection"`
	Versions          Versions           `json:"versions"`
	Context           *mapper.Context    `json:"context,omitempty"`
	Torrent           *TorrentSummary    `json:"torrent,omitempty"`
	ManifestSHA256    string             `json:"manifestSha256,omitempty"`
	Files             []FileResult       `json:"files,omitempty"`
	Blockers          []Blocker          `json:"blockers,omitempty"`
	Command           *CommandResult     `json:"command,omitempty"`
	QueueFinalization *QueueFinalization `json:"queueFinalization,omitempty"`
	Timeline          []Event            `json:"timeline"`
}

type Versions struct {
	Sonarr            string `json:"sonarr"`
	QBittorrent       string `json:"qbittorrent"`
	QBittorrentWebAPI string `json:"qbittorrentWebApi"`
}

type TorrentSummary struct {
	Hash      string `json:"hash"`
	Name      string `json:"name"`
	State     string `json:"state"`
	Category  string `json:"category"`
	FileCount int    `json:"fileCount"`
}

type FileResult struct {
	Index        int                      `json:"index"`
	RelativePath string                   `json:"relativePath"`
	Size         int64                    `json:"size"`
	Selected     bool                     `json:"selected"`
	Complete     bool                     `json:"complete"`
	Media        bool                     `json:"media"`
	Mapping      *mapper.Decision         `json:"mapping,omitempty"`
	Rename       *RenameResult            `json:"rename,omitempty"`
	SonarrPath   string                   `json:"sonarrPath,omitempty"`
	Rejections   []sonarr.ImportRejection `json:"rejections,omitempty"`
	Outcome      string                   `json:"outcome"`
	Reason       string                   `json:"reason,omitempty"`
	Verification *Verification            `json:"verification,omitempty"`
}

type RenameResult struct {
	FromPath string `json:"fromPath"`
	ToPath   string `json:"toPath"`
	Status   string `json:"status"`
}

type Verification struct {
	HistoryID     int    `json:"historyId"`
	EpisodeID     int    `json:"episodeId"`
	EpisodeFileID int    `json:"episodeFileId"`
	SourcePath    string `json:"sourcePath"`
	ImportedPath  string `json:"importedPath"`
}

type Blocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type CommandResult struct {
	ID      int    `json:"id"`
	Status  string `json:"status"`
	Result  string `json:"result,omitempty"`
	Message string `json:"message,omitempty"`
}

type QueueFinalization struct {
	Status                string `json:"status"`
	Method                string `json:"method"`
	FinalizedQueueIDs     []int  `json:"finalizedQueueIds,omitempty"`
	PendingQueueID        int    `json:"pendingQueueId,omitempty"`
	PendingChangeCategory bool   `json:"pendingChangeCategory,omitempty"`
	CategoryBefore        string `json:"categoryBefore"`
	CategoryAfter         string `json:"categoryAfter"`
}

type Event struct {
	Step    string `json:"step"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type preparedFile struct {
	resultIndex        int
	manifest           qbittorrent.File
	originalPath       string
	targetPath         string
	expectedSourcePath string
	renameApplied      bool
	candidate          sonarr.ManualImportCandidate
	commandFile        sonarr.ManualImportFile
}

type plan struct {
	result           Result
	selection        Selection
	context          mapper.Context
	outputPath       string
	queueRecords     []sonarr.QueueRecord
	episodes         []sonarr.Episode
	torrent          qbittorrent.Torrent
	manifest         []qbittorrent.File
	manifestSHA256   string
	observedCategory string
	historyBaseline  map[int]struct{}
	prepared         []preparedFile
}
