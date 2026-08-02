package prowlarr

import "time"

type IndexerCategory struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type Release struct {
	GUID        string            `json:"guid"`
	Size        int64             `json:"size"`
	Files       *int              `json:"files"`
	IndexerID   int               `json:"indexerId"`
	Indexer     string            `json:"indexer"`
	Title       string            `json:"title"`
	PublishDate time.Time         `json:"publishDate"`
	DownloadURL string            `json:"downloadUrl"`
	InfoURL     string            `json:"infoUrl"`
	Categories  []IndexerCategory `json:"categories"`
	MagnetURL   string            `json:"magnetUrl"`
	InfoHash    string            `json:"infoHash"`
	Seeders     *int              `json:"seeders"`
	Leechers    *int              `json:"leechers"`
	Protocol    string            `json:"protocol"`
}
