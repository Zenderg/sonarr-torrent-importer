package qbittorrent

type Versions struct {
	Application string `json:"application"`
	WebAPI      string `json:"webApi"`
	Libtorrent  string `json:"libtorrent"`
}

type TorrentProperties struct {
	PieceSize      int64 `json:"piece_size"`
	PiecesHave     int   `json:"pieces_have"`
	PiecesNum      int   `json:"pieces_num"`
	CompletionDate int64 `json:"completion_date"`
}

type AddTorrentOptions struct {
	SavePath string
	Category string
	Tags     string
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
	AddedOn     int64   `json:"added_on"`
	ForceStart  bool    `json:"force_start"`
}

type File struct {
	Index        int     `json:"index"`
	Name         string  `json:"name"`
	Size         int64   `json:"size"`
	Progress     float64 `json:"progress"`
	Priority     int     `json:"priority"`
	Availability float64 `json:"availability"`
}
