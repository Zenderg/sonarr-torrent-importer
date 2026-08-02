package rolling

import (
	"time"

	"github.com/zenderg/sonarr-torrent-importer/internal/sonarr"
)

const recordVersion = 1

type EnrollmentRequest struct {
	ReleaseID         string `json:"releaseId"`
	DownloadID        string `json:"downloadId"`
	ConfirmDownloadID string `json:"confirmDownloadId"`
	IndexerID         int    `json:"indexerId"`
	GUID              string `json:"guid"`
	Query             string `json:"query"`
}

type CheckRequest struct {
	ReleaseID string `json:"releaseId"`
}

type Source struct {
	Adapter   string `json:"adapter"`
	IndexerID int    `json:"indexerId"`
	GUID      string `json:"guid"`
	Query     string `json:"query"`
}

type Release struct {
	Version           int        `json:"version"`
	ID                string     `json:"id"`
	Source            Source     `json:"source"`
	SeriesID          int        `json:"seriesId"`
	SeasonNumber      int        `json:"seasonNumber"`
	SeriesTitle       string     `json:"seriesTitle"`
	QualityName       string     `json:"qualityName"`
	CurrentRevision   Revision   `json:"currentRevision"`
	CandidateRevision *Revision  `json:"candidateRevision,omitempty"`
	Operation         *Operation `json:"operation,omitempty"`
	Status            string     `json:"status"`
	BlockedReason     string     `json:"blockedReason,omitempty"`
	LastAttemptAt     time.Time  `json:"lastAttemptAt,omitempty"`
	LastAttemptError  string     `json:"lastAttemptError,omitempty"`
	LastCheckedAt     time.Time  `json:"lastCheckedAt,omitempty"`
	NextCheckAt       time.Time  `json:"nextCheckAt,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

type Revision struct {
	ID             string         `json:"id"`
	TorrentID      string         `json:"torrentId"`
	InfoHashV1     string         `json:"infoHashV1,omitempty"`
	InfoHashV2     string         `json:"infoHashV2,omitempty"`
	ArtifactSHA256 string         `json:"artifactSha256"`
	Name           string         `json:"name"`
	PieceLength    int64          `json:"pieceLength"`
	TotalLength    int64          `json:"totalLength"`
	ReusedBytes    int64          `json:"reusedBytes,omitempty"`
	SavePath       string         `json:"savePath"`
	SonarrSavePath string         `json:"sonarrSavePath"`
	Category       string         `json:"category"`
	Tags           string         `json:"tags,omitempty"`
	AddedOn        int64          `json:"addedOn"`
	Files          []RevisionFile `json:"files"`
	ObservedAt     time.Time      `json:"observedAt"`
}

type RevisionFile struct {
	Index         int    `json:"index"`
	RawPath       string `json:"rawPath"`
	CurrentPath   string `json:"currentPath"`
	CanonicalPath string `json:"canonicalPath,omitempty"`
	Size          int64  `json:"size"`
	EpisodeID     int    `json:"episodeId,omitempty"`
	EpisodeNumber int    `json:"episodeNumber,omitempty"`
	EpisodeFileID int    `json:"episodeFileId,omitempty"`
	LibraryPath   string `json:"libraryPath,omitempty"`
	ContentSHA256 string `json:"contentSha256,omitempty"`
	Copied        bool   `json:"copied,omitempty"`
	RenameApplied bool   `json:"renameApplied,omitempty"`
	ImportNeeded  bool   `json:"importNeeded,omitempty"`
	Imported      bool   `json:"imported,omitempty"`
	HistoryID     int    `json:"historyId,omitempty"`
}

type Operation struct {
	ID                string                    `json:"id"`
	Phase             string                    `json:"phase"`
	PlanToken         string                    `json:"planToken"`
	OldTorrentID      string                    `json:"oldTorrentId"`
	NewTorrentID      string                    `json:"newTorrentId"`
	HistoryBaseline   []int                     `json:"historyBaseline,omitempty"`
	CommandID         int                       `json:"commandId,omitempty"`
	CommandAcceptedAt time.Time                 `json:"commandAcceptedAt,omitempty"`
	ImportFiles       []sonarr.ManualImportFile `json:"importFiles,omitempty"`
	MutationFileIndex int                       `json:"mutationFileIndex,omitempty"`
	StartedAt         time.Time                 `json:"startedAt"`
	UpdatedAt         time.Time                 `json:"updatedAt"`
}

type CheckResult struct {
	Release Release `json:"release"`
	Changed bool    `json:"changed"`
	Message string  `json:"message"`
}
