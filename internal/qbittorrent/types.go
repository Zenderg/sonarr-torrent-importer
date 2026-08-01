package qbittorrent

type Versions struct {
	Application string `json:"application"`
	WebAPI      string `json:"webApi"`
}

type Torrent struct {
	Hash        string  `json:"hash"`
	Name        string  `json:"name"`
	Size        int64   `json:"size"`
	Progress    float64 `json:"progress"`
	State       string  `json:"state"`
	SavePath    string  `json:"save_path"`
	ContentPath string  `json:"content_path"`
	Category    string  `json:"category"`
	Tags        string  `json:"tags"`
	InfoHashV1  string  `json:"infohash_v1"`
	InfoHashV2  string  `json:"infohash_v2"`
}

type File struct {
	Index        int     `json:"index"`
	Name         string  `json:"name"`
	Size         int64   `json:"size"`
	Progress     float64 `json:"progress"`
	Priority     int     `json:"priority"`
	Availability float64 `json:"availability"`
}
