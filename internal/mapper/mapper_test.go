package mapper

import "testing"

func TestMapPhaseZeroConvention(t *testing.T) {
	context := Context{SeriesID: 101, SeasonNumber: 2, QueueIDs: []int{44}, DownloadID: "0123456789012345678901234567890123456789", Source: "sonarrQueue"}
	episodes := []Episode{
		{ID: 1103, SeriesID: 101, SeasonNumber: 1, EpisodeNumber: 3},
		{ID: 2201, SeriesID: 101, SeasonNumber: 2, EpisodeNumber: 1},
		{ID: 2203, SeriesID: 101, SeasonNumber: 2, EpisodeNumber: 3},
	}

	tests := []struct {
		name      string
		file      string
		status    string
		reason    string
		episodeID int
	}{
		{name: "exact", file: "Clockwork Garden/[03].mkv", status: "mapped", episodeID: 2203},
		{name: "uppercase extension", file: "[03].MKV", status: "mapped", episodeID: 2203},
		{name: "one digit", file: "[3].mkv", status: "blocked", reason: "FILENAME_PATTERN_MISMATCH"},
		{name: "three digits", file: "[003].mkv", status: "blocked", reason: "FILENAME_PATTERN_MISMATCH"},
		{name: "suffix", file: "[03] sample.mkv", status: "blocked", reason: "FILENAME_PATTERN_MISMATCH"},
		{name: "zero", file: "[00].mkv", status: "blocked", reason: "LOCAL_EPISODE_ZERO"},
		{name: "no inference", file: "[04].mkv", status: "blocked", reason: "NO_EPISODE_IN_CONFIRMED_CONTEXT"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := Map([]File{{RelativePath: test.file, Size: 42}}, context, episodes)
			if len(result.Decisions) != 1 {
				t.Fatalf("got %d decisions, want 1", len(result.Decisions))
			}
			decision := result.Decisions[0]
			if decision.Status != test.status || decision.Reason != test.reason {
				t.Fatalf("got status=%q reason=%q, want status=%q reason=%q", decision.Status, decision.Reason, test.status, test.reason)
			}
			if test.episodeID != 0 && (len(decision.EpisodeIDs) != 1 || decision.EpisodeIDs[0] != test.episodeID) {
				t.Fatalf("got episode IDs %v, want [%d]", decision.EpisodeIDs, test.episodeID)
			}
		})
	}
}

func TestMapBlocksDuplicateTarget(t *testing.T) {
	context := Context{SeriesID: 101, SeasonNumber: 2, QueueIDs: []int{44}, DownloadID: "0123456789012345678901234567890123456789", Source: "sonarrQueue"}
	result := Map(
		[]File{{RelativePath: "Disc A/[03].mkv"}, {RelativePath: "Disc B/[03].mkv"}},
		context,
		[]Episode{{ID: 2203, SeriesID: 101, SeasonNumber: 2, EpisodeNumber: 3}},
	)
	if result.CanExecute {
		t.Fatal("duplicate target unexpectedly executable")
	}
	for _, decision := range result.Decisions {
		if decision.Reason != "DUPLICATE_EPISODE_TARGET" {
			t.Fatalf("got reason %q, want DUPLICATE_EPISODE_TARGET", decision.Reason)
		}
	}
}

func TestMapBlocksConflictingEpisodeMetadata(t *testing.T) {
	context := Context{SeriesID: 101, SeasonNumber: 2, QueueIDs: []int{44}, DownloadID: "0123456789012345678901234567890123456789", Source: "sonarrQueue"}
	result := Map(
		[]File{{RelativePath: "[03].mkv"}},
		context,
		[]Episode{
			{ID: 2203, SeriesID: 101, SeasonNumber: 2, EpisodeNumber: 3},
			{ID: 2203, SeriesID: 101, SeasonNumber: 2, EpisodeNumber: 4},
		},
	)
	if result.Decisions[0].Reason != "INCONSISTENT_EPISODE_METADATA" {
		t.Fatalf("got reason %q", result.Decisions[0].Reason)
	}
}
