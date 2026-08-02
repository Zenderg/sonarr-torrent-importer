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

func TestCorrelateCandidateAllowsUnparsedSeasonForExplicitMapping(t *testing.T) {
	context := mapper.Context{SeriesID: 101, SeasonNumber: 2, DownloadID: testDownloadID}
	candidate := sonarr.ManualImportCandidate{
		Path: "/downloads/[03].mkv", RelativePath: "[03].mkv", Size: 100,
		Series: &sonarr.SeriesRef{ID: 101}, SeasonNumber: nil, DownloadID: testDownloadID,
	}

	matched, err := correlateCandidate(
		qbittorrent.File{Name: "[03].mkv", Size: 100},
		[]sonarr.ManualImportCandidate{candidate},
		context,
		"/downloads/[03].mkv",
	)
	if err != nil {
		t.Fatal(err)
	}
	if matched.Path != candidate.Path {
		t.Fatalf("got %q, want %q", matched.Path, candidate.Path)
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
		manifest:           qbittorrent.File{Name: "Clockwork Garden/[03].mkv", Size: 100},
		expectedSourcePath: "/downloads/Clockwork Garden/[03].mkv",
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
	history[0].Data["droppedPath"] = "/other-root/Clockwork Garden/[03].mkv"
	if _, ok := verifyEvidence(prepared, episode, files, history, nil); ok {
		t.Fatal("matching relative suffix from an unbound source root was accepted")
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

func TestCanonicalTorrentPathUsesConfirmedSonarrMetadata(t *testing.T) {
	target, err := canonicalTorrentPath(
		"Futurama.Release/[01].mkv",
		"Futurama",
		"WEBDL-720p",
		sonarr.Episode{SeasonNumber: 1, EpisodeNumber: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if target != "Futurama.Release/Futurama.S01E01.WEBDL-720p.mkv" {
		t.Fatalf("target = %q", target)
	}
	if _, err := canonicalTorrentPath("[01].mkv", "Futurama", "WEBDL-720p", sonarr.Episode{SeasonNumber: 1, EpisodeNumber: 1}); err == nil {
		t.Fatal("single-file torrent rename unexpectedly accepted")
	}
}

func TestValidateRenamePlanRejectsOccupiedAndCaseFoldedTargets(t *testing.T) {
	manifest := []qbittorrent.File{
		{Index: 0, Name: "Release/[01].mkv"},
		{Index: 1, Name: "Release/Futurama.S01E01.WEBDL-720p.mkv"},
	}
	prepared := []preparedFile{{
		manifest: manifest[0], originalPath: manifest[0].Name,
		targetPath: "Release/futurama.s01e01.webdl-720p.mkv",
	}}
	if err := validateRenamePlan(manifest, prepared); err == nil {
		t.Fatal("case-folded occupied rename target unexpectedly accepted")
	}
}

func TestValidTorrentPathRejectsTraversalAndControlCharacters(t *testing.T) {
	invalid := []string{"", "/absolute.mkv", "../episode.mkv", "dir/../episode.mkv", "dir//episode.mkv", `dir\episode.mkv`, "dir/episode\x00.mkv", "dir/episode\n.mkv", "C:/episode.mkv", "C:episode.mkv"}
	for _, value := range invalid {
		if validTorrentPath(value) {
			t.Errorf("invalid torrent path %q was accepted", value)
		}
	}
	if !validTorrentPath("Release/[01].mkv") {
		t.Fatal("valid torrent path was rejected")
	}
}

func TestTorrentContentPathAllowsOnlyThePlannedFileRename(t *testing.T) {
	original := qbittorrent.File{Index: 0, Name: "Release/[01].mkv"}
	target := original
	target.Name = "Release/Futurama.S01E01.WEBDL-720p.mkv"
	built := plan{
		torrent:  qbittorrent.Torrent{ContentPath: "/downloads/Release/[01].mkv"},
		prepared: []preparedFile{{manifest: original, originalPath: original.Name, targetPath: target.Name}},
	}
	current := qbittorrent.Torrent{ContentPath: "/downloads/Release/Futurama.S01E01.WEBDL-720p.mkv"}
	if err := validateTorrentContentPath(&built, current, []qbittorrent.File{target}); err != nil {
		t.Fatalf("planned content_path transition was rejected: %v", err)
	}
	current.ContentPath = "/other/Release/Futurama.S01E01.WEBDL-720p.mkv"
	if err := validateTorrentContentPath(&built, current, []qbittorrent.File{target}); err == nil {
		t.Fatal("unplanned content_path transition was accepted")
	}
}

func TestSonarrPathAfterRenameReplacesStaleQueueFilePath(t *testing.T) {
	prepared := []preparedFile{{
		originalPath: "Release/[01].mkv",
		targetPath:   "Release/Futurama.S01E01.WEBDL-720p.mkv",
	}}
	actual, err := sonarrPathAfterRenames("/media/downloads/Release/[01].mkv", prepared)
	if err != nil {
		t.Fatal(err)
	}
	expected := "/media/downloads/Release/Futurama.S01E01.WEBDL-720p.mkv"
	if actual != expected {
		t.Fatalf("translated Sonarr path = %q, want %q", actual, expected)
	}
}
