package sonarr

import "encoding/json"

type SystemStatus struct {
	Version string `json:"version"`
	Branch  string `json:"branch"`
}

type QueuePage struct {
	Page         int           `json:"page"`
	PageSize     int           `json:"pageSize"`
	TotalRecords int           `json:"totalRecords"`
	Records      []QueueRecord `json:"records"`
}

type QueueRecord struct {
	ID                                  int        `json:"id"`
	SeriesID                            *int       `json:"seriesId"`
	EpisodeID                           *int       `json:"episodeId"`
	SeasonNumber                        *int       `json:"seasonNumber"`
	Title                               string     `json:"title"`
	Status                              string     `json:"status"`
	TrackedDownloadStatus               string     `json:"trackedDownloadStatus"`
	TrackedDownloadState                string     `json:"trackedDownloadState"`
	DownloadID                          string     `json:"downloadId"`
	Protocol                            string     `json:"protocol"`
	DownloadClient                      string     `json:"downloadClient"`
	DownloadClientHasPostImportCategory bool       `json:"downloadClientHasPostImportCategory"`
	OutputPath                          string     `json:"outputPath"`
	Series                              *SeriesRef `json:"series"`
	Episode                             *Episode   `json:"episode"`
}

type SeriesRef struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

type Episode struct {
	ID                    int          `json:"id"`
	SeriesID              int          `json:"seriesId"`
	EpisodeFileID         int          `json:"episodeFileId"`
	SeasonNumber          int          `json:"seasonNumber"`
	EpisodeNumber         int          `json:"episodeNumber"`
	Title                 string       `json:"title"`
	HasFile               bool         `json:"hasFile"`
	Monitored             bool         `json:"monitored"`
	AbsoluteEpisodeNumber *int         `json:"absoluteEpisodeNumber"`
	EpisodeFile           *EpisodeFile `json:"episodeFile"`
}

type EpisodeFile struct {
	ID           int    `json:"id"`
	SeriesID     int    `json:"seriesId"`
	SeasonNumber int    `json:"seasonNumber"`
	RelativePath string `json:"relativePath"`
	Path         string `json:"path"`
	Size         int64  `json:"size"`
}

type ImportRejection struct {
	Reason string `json:"reason"`
	Type   string `json:"type"`
}

type ManualImportCandidate struct {
	ID           int               `json:"id"`
	Path         string            `json:"path"`
	RelativePath string            `json:"relativePath"`
	FolderName   string            `json:"folderName"`
	Name         string            `json:"name"`
	Size         int64             `json:"size"`
	Series       *SeriesRef        `json:"series"`
	SeasonNumber *int              `json:"seasonNumber"`
	Episodes     []Episode         `json:"episodes"`
	ReleaseGroup string            `json:"releaseGroup"`
	Quality      json.RawMessage   `json:"quality"`
	Languages    json.RawMessage   `json:"languages"`
	DownloadID   string            `json:"downloadId"`
	IndexerFlags int               `json:"indexerFlags"`
	ReleaseType  json.RawMessage   `json:"releaseType"`
	Rejections   []ImportRejection `json:"rejections"`
}

type ManualImportReprocess struct {
	Path         string            `json:"path"`
	SeriesID     int               `json:"seriesId"`
	SeasonNumber *int              `json:"seasonNumber,omitempty"`
	Episodes     []Episode         `json:"episodes,omitempty"`
	EpisodeIDs   []int             `json:"episodeIds,omitempty"`
	Quality      json.RawMessage   `json:"quality"`
	Languages    json.RawMessage   `json:"languages"`
	ReleaseGroup string            `json:"releaseGroup"`
	DownloadID   string            `json:"downloadId"`
	IndexerFlags int               `json:"indexerFlags"`
	ReleaseType  json.RawMessage   `json:"releaseType"`
	Rejections   []ImportRejection `json:"rejections,omitempty"`
}

type ManualImportFile struct {
	Path         string          `json:"path"`
	FolderName   string          `json:"folderName"`
	SeriesID     int             `json:"seriesId"`
	EpisodeIDs   []int           `json:"episodeIds"`
	Quality      json.RawMessage `json:"quality"`
	Languages    json.RawMessage `json:"languages"`
	ReleaseGroup string          `json:"releaseGroup"`
	IndexerFlags int             `json:"indexerFlags"`
	ReleaseType  json.RawMessage `json:"releaseType"`
	DownloadID   string          `json:"downloadId"`
}

type ManualImportCommand struct {
	Name       string             `json:"name"`
	Files      []ManualImportFile `json:"files"`
	ImportMode string             `json:"importMode"`
}

type Command struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Result    string `json:"result"`
	Message   string `json:"message"`
	Exception string `json:"exception"`
}

type HistoryPage struct {
	TotalRecords int             `json:"totalRecords"`
	Records      []HistoryRecord `json:"records"`
}

type HistoryRecord struct {
	ID          int               `json:"id"`
	EpisodeID   int               `json:"episodeId"`
	SeriesID    int               `json:"seriesId"`
	SourceTitle string            `json:"sourceTitle"`
	DownloadID  string            `json:"downloadId"`
	EventType   string            `json:"eventType"`
	Data        map[string]string `json:"data"`
}
