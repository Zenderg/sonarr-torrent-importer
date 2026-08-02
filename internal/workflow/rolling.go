package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/zenderg/sonarr-torrent-importer/internal/qbittorrent"
	"github.com/zenderg/sonarr-torrent-importer/internal/sonarr"
)

// RollingEvidence is immutable enrollment evidence derived from a completed,
// queue-confirmed import. Callers cannot supply series, season, naming, or
// ownership facts themselves.
type RollingEvidence struct {
	DownloadID   string                `json:"downloadId"`
	SeriesID     int                   `json:"seriesId"`
	SeasonNumber int                   `json:"seasonNumber"`
	SeriesTitle  string                `json:"seriesTitle"`
	QualityName  string                `json:"qualityName"`
	Torrent      qbittorrent.Torrent   `json:"torrent"`
	Files        []RollingEvidenceFile `json:"files"`
}

type RollingEvidenceFile struct {
	Index         int    `json:"index"`
	OriginalPath  string `json:"originalPath"`
	CanonicalPath string `json:"canonicalPath"`
	Size          int64  `json:"size"`
	EpisodeID     int    `json:"episodeId"`
	EpisodeFileID int    `json:"episodeFileId"`
	ImportedPath  string `json:"importedPath"`
}

// RollingEvidence returns trusted context only for a fully completed durable
// operation and verifies that qBittorrent still owns the exact completed
// canonical source before enrollment.
func (e *Engine) RollingEvidence(ctx context.Context, downloadID string) (RollingEvidence, error) {
	if !validTorrentID(downloadID) {
		return RollingEvidence{}, fmt.Errorf("downloadId is not a valid torrent identity")
	}
	record, exists, err := e.operations.load(downloadID)
	if err != nil {
		return RollingEvidence{}, err
	}
	if !exists || record.Phase != "complete" || record.Plan.Result.Outcome != "imported" {
		return RollingEvidence{}, fmt.Errorf("rolling enrollment requires a completed imported operation for downloadId %q", downloadID)
	}
	seriesTitle, qualityName, err := queueRenameMetadata(record.Plan.QueueRecords)
	if err != nil {
		return RollingEvidence{}, fmt.Errorf("recover canonical naming evidence: %w", err)
	}
	if err := e.qbittorrent.Login(ctx); err != nil {
		return RollingEvidence{}, fmt.Errorf("authenticate with qBittorrent: %w", err)
	}
	torrent, err := e.qbittorrent.Torrent(ctx, downloadID)
	if err != nil {
		return RollingEvidence{}, fmt.Errorf("read enrolled qBittorrent torrent: %w", err)
	}
	if !activeSeedingState(torrent.State) || torrent.Progress != 1 {
		return RollingEvidence{}, fmt.Errorf("enrolled torrent is not complete and actively seeding")
	}
	manifest, err := e.qbittorrent.Files(ctx, torrent.Hash)
	if err != nil {
		return RollingEvidence{}, fmt.Errorf("read enrolled qBittorrent manifest: %w", err)
	}
	byIndex := make(map[int]qbittorrent.File, len(manifest))
	for _, file := range manifest {
		byIndex[file.Index] = file
	}
	evidence := RollingEvidence{
		DownloadID: strings.ToLower(record.Plan.Context.DownloadID),
		SeriesID:   record.Plan.Context.SeriesID, SeasonNumber: record.Plan.Context.SeasonNumber,
		SeriesTitle: seriesTitle, QualityName: qualityName, Torrent: torrent,
		Files: make([]RollingEvidenceFile, 0, len(record.Plan.Prepared)),
	}
	for _, prepared := range record.Plan.Prepared {
		current, ok := byIndex[prepared.Manifest.Index]
		if !ok || current.Name != prepared.TargetPath || current.Size != prepared.Manifest.Size || current.Progress != 1 || current.Priority == 0 {
			return RollingEvidence{}, fmt.Errorf("canonical qBittorrent file %d no longer matches completed enrollment evidence", prepared.Manifest.Index)
		}
		result := record.Plan.Result.Files[prepared.ResultIndex]
		if result.Mapping == nil || len(result.Mapping.EpisodeIDs) != 1 || result.Verification == nil || result.Verification.EpisodeFileID <= 0 {
			return RollingEvidence{}, fmt.Errorf("completed file %d lacks exact Sonarr ownership evidence", prepared.Manifest.Index)
		}
		evidence.Files = append(evidence.Files, RollingEvidenceFile{
			Index: current.Index, OriginalPath: prepared.OriginalPath, CanonicalPath: current.Name,
			Size: current.Size, EpisodeID: result.Mapping.EpisodeIDs[0],
			EpisodeFileID: result.Verification.EpisodeFileID, ImportedPath: result.Verification.ImportedPath,
		})
	}
	if len(evidence.Files) == 0 {
		return RollingEvidence{}, fmt.Errorf("completed operation has no owned media files")
	}
	return evidence, nil
}

// CanonicalTorrentPath applies the release naming contract shared by normal
// and rolling imports.
func CanonicalTorrentPath(source, seriesTitle, qualityName string, episodeNumber, seasonNumber int) (string, error) {
	return canonicalTorrentPath(source, seriesTitle, qualityName, sonarrEpisode(episodeNumber, seasonNumber))
}

func sonarrEpisode(episodeNumber, seasonNumber int) sonarr.Episode {
	return sonarr.Episode{EpisodeNumber: episodeNumber, SeasonNumber: seasonNumber}
}
