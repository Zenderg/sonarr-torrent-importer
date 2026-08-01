package workflow

import (
	"testing"

	"github.com/zenderg/sonarr-torrent-importer/internal/mapper"
	"github.com/zenderg/sonarr-torrent-importer/internal/qbittorrent"
	"github.com/zenderg/sonarr-torrent-importer/internal/sonarr"
)

const testDownloadID = "0123456789012345678901234567890123456789"

func TestResolveQueueSelectionAggregatesSeasonPack(t *testing.T) {
	seriesID := 101
	season := 2
	records := []sonarr.QueueRecord{
		{ID: 12, DownloadID: testDownloadID, Protocol: "torrent", SeriesID: &seriesID, SeasonNumber: &season, OutputPath: "/downloads/Clockwork Garden", DownloadClient: "qBittorrent"},
		{ID: 11, DownloadID: testDownloadID, Protocol: "torrent", SeriesID: &seriesID, SeasonNumber: &season, OutputPath: "/downloads/Clockwork Garden", DownloadClient: "qBittorrent"},
	}

	matched, context, outputPath, err := resolveQueueSelection(records, Selection{DownloadID: testDownloadID})
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 2 || context.SeriesID != seriesID || context.SeasonNumber != season || outputPath != "/downloads/Clockwork Garden" {
		t.Fatalf("unexpected resolution: matched=%d context=%+v outputPath=%q", len(matched), context, outputPath)
	}
	if len(context.QueueIDs) != 2 || context.QueueIDs[0] != 11 || context.QueueIDs[1] != 12 {
		t.Fatalf("queue IDs not stable and sorted: %v", context.QueueIDs)
	}
}

func TestResolveQueueSelectionBlocksMixedSeason(t *testing.T) {
	seriesID := 101
	seasonTwo, seasonThree := 2, 3
	records := []sonarr.QueueRecord{
		{ID: 11, DownloadID: testDownloadID, Protocol: "torrent", SeriesID: &seriesID, SeasonNumber: &seasonTwo, OutputPath: "/downloads/Clockwork Garden", DownloadClient: "qBittorrent"},
		{ID: 12, DownloadID: testDownloadID, Protocol: "torrent", SeriesID: &seriesID, SeasonNumber: &seasonThree, OutputPath: "/downloads/Clockwork Garden", DownloadClient: "qBittorrent"},
	}
	if _, _, _, err := resolveQueueSelection(records, Selection{DownloadID: testDownloadID}); err == nil {
		t.Fatal("mixed season context unexpectedly accepted")
	}
}

func TestCorrelateCandidateRequiresPathSizeAndContext(t *testing.T) {
	season := 2
	context := mapper.Context{SeriesID: 101, SeasonNumber: season, DownloadID: testDownloadID}
	candidates := []sonarr.ManualImportCandidate{
		{Path: "/downloads/Clockwork Garden/[03].mkv", RelativePath: "[03].mkv", Size: 100, Series: &sonarr.SeriesRef{ID: 101}, SeasonNumber: &season, DownloadID: testDownloadID},
		{Path: "/downloads/Clockwork Garden/[04].mkv", RelativePath: "[04].mkv", Size: 100, Series: &sonarr.SeriesRef{ID: 101}, SeasonNumber: &season, DownloadID: testDownloadID},
	}
	candidate, err := correlateCandidate(
		qbittorrent.File{Name: "Clockwork Garden/[03].mkv", Size: 100},
		candidates,
		context,
		"/downloads/Clockwork Garden",
	)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Path != candidates[0].Path {
		t.Fatalf("got %q, want %q", candidate.Path, candidates[0].Path)
	}
}

func TestManifestDigestIgnoresAvailabilityButDetectsIdentityChanges(t *testing.T) {
	torrent := qbittorrent.Torrent{Hash: testDownloadID, Name: "Clockwork Garden"}
	files := []qbittorrent.File{{Index: 0, Name: "[03].mkv", Size: 100, Progress: 1, Priority: 1, Availability: 0.25}}
	first, err := manifestDigest(torrent, files)
	if err != nil {
		t.Fatal(err)
	}
	files[0].Availability = 1
	second, _ := manifestDigest(torrent, files)
	if first != second {
		t.Fatal("peer availability changed the immutable manifest digest")
	}
	files[0].Size++
	third, _ := manifestDigest(torrent, files)
	if first == third {
		t.Fatal("file identity change did not change manifest digest")
	}
}

func TestVerifyEvidenceRequiresMatchingHistoryAndEpisodeFile(t *testing.T) {
	season := 2
	prepared := preparedFile{
		manifest: qbittorrent.File{Name: "Clockwork Garden/[03].mkv", Size: 100},
		candidate: sonarr.ManualImportCandidate{
			Path: "/downloads/Clockwork Garden/[03].mkv", DownloadID: testDownloadID,
		},
	}
	episode := sonarr.Episode{ID: 2203, SeriesID: 101, SeasonNumber: season, HasFile: true, EpisodeFileID: 700}
	files := map[int]sonarr.EpisodeFile{700: {ID: 700, SeriesID: 101, SeasonNumber: season, Path: "/library/Clockwork Garden/Season 02/E03.mkv", Size: 100}}
	history := []sonarr.HistoryRecord{{
		ID: 900, EpisodeID: 2203, SeriesID: 101, DownloadID: testDownloadID,
		EventType: "downloadFolderImported",
		Data: map[string]string{
			"droppedPath":  "/downloads/Clockwork Garden/[03].mkv",
			"fileId":       "700",
			"importedPath": "/library/Clockwork Garden/Season 02/E03.mkv",
		},
	}}

	verification, ok := verifyEvidence(prepared, episode, files, history, nil)
	if !ok || verification.HistoryID != 900 || verification.EpisodeFileID != 700 {
		t.Fatalf("valid evidence was not accepted: ok=%v verification=%+v", ok, verification)
	}
	if _, ok := verifyEvidence(prepared, episode, files, history, map[int]struct{}{900: {}}); ok {
		t.Fatal("baseline history was accepted as a new import postcondition")
	}
}

func TestPlanTokenUsesResolvedDownloadIdentityAndSafetySnapshot(t *testing.T) {
	seriesID, season := 101, 2
	queueRecord := sonarr.QueueRecord{
		ID: 11, SeriesID: &seriesID, SeasonNumber: &season, DownloadID: testDownloadID,
		Protocol: "torrent", OutputPath: "/downloads/Clockwork Garden", DownloadClient: "qBittorrent",
	}
	built := plan{
		selection:       Selection{DownloadID: testDownloadID, QueueID: 11},
		context:         mapper.Context{SeriesID: seriesID, SeasonNumber: season, QueueIDs: []int{11}, DownloadID: testDownloadID, Source: "sonarrQueue"},
		outputPath:      queueRecord.OutputPath,
		queueRecords:    []sonarr.QueueRecord{queueRecord},
		torrent:         qbittorrent.Torrent{Hash: testDownloadID, Name: "Clockwork Garden", State: "uploading", Category: "sonarr"},
		manifestSHA256:  "manifest",
		historyBaseline: map[int]struct{}{},
	}
	fromQueueID, err := calculatePlanToken(built)
	if err != nil {
		t.Fatal(err)
	}
	built.selection.QueueID = 0
	fromDownloadID, err := calculatePlanToken(built)
	if err != nil {
		t.Fatal(err)
	}
	if fromQueueID != fromDownloadID {
		t.Fatal("queueId dry-run and resolved downloadId execute produced different plan tokens")
	}
	built.queueRecords[0].OutputPath = "/downloads/changed"
	changed, err := calculatePlanToken(built)
	if err != nil {
		t.Fatal(err)
	}
	if changed == fromDownloadID {
		t.Fatal("queue safety change did not invalidate the plan token")
	}
}
