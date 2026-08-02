package mapper

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	MapperID      = "season-relative-bracketed"
	MapperVersion = 1
	RuleID        = "season-relative-bracketed-two-digit-mkv"
	RuleVersion   = 1
)

var bracketedEpisodePattern = regexp.MustCompile(`(?i)^\[([0-9]{2})\]\.mkv$`)

type Context struct {
	SeriesID     int    `json:"seriesId"`
	SeasonNumber int    `json:"seasonNumber"`
	QueueIDs     []int  `json:"queueIds"`
	DownloadID   string `json:"downloadId"`
	Source       string `json:"source"`
}

type File struct {
	RelativePath string `json:"relativePath"`
	Size         int64  `json:"size"`
}

type Episode struct {
	ID            int    `json:"id"`
	SeriesID      int    `json:"seriesId"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	Title         string `json:"title,omitempty"`
}

type Result struct {
	Mapper     Version    `json:"mapper"`
	CanExecute bool       `json:"canExecute"`
	Decisions  []Decision `json:"decisions"`
}

type Version struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

type Decision struct {
	Status              string   `json:"status"`
	RelativePath        string   `json:"relativePath"`
	SeriesID            int      `json:"seriesId,omitempty"`
	SeasonNumber        int      `json:"seasonNumber,omitempty"`
	EpisodeIDs          []int    `json:"episodeIds,omitempty"`
	CandidateEpisodeIDs []int    `json:"candidateEpisodeIds,omitempty"`
	Confidence          string   `json:"confidence"`
	Reason              string   `json:"reason,omitempty"`
	Rule                *Version `json:"rule,omitempty"`
	Evidence            Evidence `json:"evidence"`
	Explanation         string   `json:"explanation"`
}

type Evidence struct {
	FilenamePattern          string   `json:"filenamePattern,omitempty"`
	ParsedLocalEpisodeNumber *int     `json:"parsedLocalEpisodeNumber,omitempty"`
	Context                  *Context `json:"context,omitempty"`
	MatchedEpisodeID         *int     `json:"matchedEpisodeId,omitempty"`
}

func Map(files []File, context Context, episodes []Episode) Result {
	result := Result{
		Mapper:    Version{ID: MapperID, Version: MapperVersion},
		Decisions: make([]Decision, len(files)),
	}

	if reason := validateContext(context); reason != "" {
		for index, file := range files {
			result.Decisions[index] = blocked(file.RelativePath, reason, nil, Evidence{}, "Confirmed Sonarr series/season context is invalid.")
		}
		return result
	}

	if inconsistentEpisodeMetadata(episodes) {
		for index, file := range files {
			result.Decisions[index] = blocked(file.RelativePath, "INCONSISTENT_EPISODE_METADATA", nil, Evidence{Context: &context}, "Sonarr returned contradictory metadata for the same episode ID.")
		}
		return result
	}

	pathCounts := make(map[string]int, len(files))
	for _, file := range files {
		pathCounts[file.RelativePath]++
	}

	for index, file := range files {
		if !validRelativePath(file.RelativePath) {
			result.Decisions[index] = blocked(file.RelativePath, "INVALID_INPUT_PATH", nil, Evidence{Context: &context}, "The torrent file path is not a safe normalized relative POSIX path.")
			continue
		}
		if pathCounts[file.RelativePath] > 1 {
			result.Decisions[index] = blocked(file.RelativePath, "DUPLICATE_INPUT_PATH", nil, Evidence{Context: &context}, "The torrent manifest contains this relative path more than once.")
			continue
		}

		matches := bracketedEpisodePattern.FindStringSubmatch(path.Base(file.RelativePath))
		if matches == nil {
			result.Decisions[index] = blocked(file.RelativePath, "FILENAME_PATTERN_MISMATCH", nil, Evidence{Context: &context}, "The filename does not exactly match the supported [NN].mkv convention.")
			continue
		}

		localNumber, _ := strconv.Atoi(matches[1])
		evidence := Evidence{
			FilenamePattern:          "bracketed-two-digit-mkv",
			ParsedLocalEpisodeNumber: intPointer(localNumber),
			Context:                  &context,
		}
		if localNumber == 0 {
			result.Decisions[index] = blocked(file.RelativePath, "LOCAL_EPISODE_ZERO", nil, evidence, "The local episode number [00] is not valid.")
			continue
		}

		candidateIDs := matchingEpisodeIDs(episodes, context, localNumber)
		switch len(candidateIDs) {
		case 0:
			result.Decisions[index] = blocked(file.RelativePath, "NO_EPISODE_IN_CONFIRMED_CONTEXT", nil, evidence, fmt.Sprintf("No Sonarr episode matches local episode %d in confirmed series %d season %d.", localNumber, context.SeriesID, context.SeasonNumber))
		case 1:
			episodeID := candidateIDs[0]
			evidence.MatchedEpisodeID = intPointer(episodeID)
			result.Decisions[index] = Decision{
				Status:       "mapped",
				RelativePath: file.RelativePath,
				SeriesID:     context.SeriesID,
				SeasonNumber: context.SeasonNumber,
				EpisodeIDs:   []int{episodeID},
				Confidence:   "exact",
				Rule:         &Version{ID: RuleID, Version: RuleVersion},
				Evidence:     evidence,
				Explanation:  fmt.Sprintf("Filename %s yielded local episode %d; confirmed context is series %d season %d; exactly one Sonarr episode matched: %d.", path.Base(file.RelativePath), localNumber, context.SeriesID, context.SeasonNumber, episodeID),
			}
		default:
			result.Decisions[index] = blocked(file.RelativePath, "MULTIPLE_EPISODES_IN_CONFIRMED_CONTEXT", candidateIDs, evidence, fmt.Sprintf("Multiple Sonarr episodes match local episode %d in confirmed series %d season %d.", localNumber, context.SeriesID, context.SeasonNumber))
		}
	}

	targets := make(map[int][]int)
	for index, decision := range result.Decisions {
		if decision.Status == "mapped" {
			targets[decision.EpisodeIDs[0]] = append(targets[decision.EpisodeIDs[0]], index)
		}
	}
	for episodeID, indexes := range targets {
		if len(indexes) < 2 {
			continue
		}
		for _, index := range indexes {
			decision := result.Decisions[index]
			result.Decisions[index] = blocked(decision.RelativePath, "DUPLICATE_EPISODE_TARGET", []int{episodeID}, decision.Evidence, fmt.Sprintf("Multiple torrent files map to Sonarr episode %d.", episodeID))
		}
	}

	result.CanExecute = len(files) > 0
	for _, decision := range result.Decisions {
		if decision.Status != "mapped" {
			result.CanExecute = false
			break
		}
	}
	return result
}

func validateContext(context Context) string {
	if context.SeriesID <= 0 || context.SeasonNumber < 0 || context.DownloadID == "" || len(context.QueueIDs) == 0 || context.Source != "sonarrQueue" {
		return "INVALID_CONFIRMED_CONTEXT"
	}
	return ""
}

func validRelativePath(value string) bool {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, `\`) || strings.HasPrefix(value, "/") {
		return false
	}
	if path.Clean(value) != value || value == "." {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func inconsistentEpisodeMetadata(episodes []Episode) bool {
	byID := make(map[int]Episode, len(episodes))
	for _, episode := range episodes {
		if episode.ID <= 0 || episode.SeriesID <= 0 || episode.SeasonNumber < 0 || episode.EpisodeNumber < 0 {
			return true
		}
		if previous, exists := byID[episode.ID]; exists {
			if previous.SeriesID != episode.SeriesID || previous.SeasonNumber != episode.SeasonNumber || previous.EpisodeNumber != episode.EpisodeNumber {
				return true
			}
		}
		byID[episode.ID] = episode
	}
	return false
}

func matchingEpisodeIDs(episodes []Episode, context Context, localNumber int) []int {
	unique := make(map[int]struct{})
	for _, episode := range episodes {
		if episode.SeriesID == context.SeriesID && episode.SeasonNumber == context.SeasonNumber && episode.EpisodeNumber == localNumber {
			unique[episode.ID] = struct{}{}
		}
	}
	result := make([]int, 0, len(unique))
	for id := range unique {
		result = append(result, id)
	}
	sort.Ints(result)
	return result
}

func blocked(relativePath, reason string, candidateIDs []int, evidence Evidence, explanation string) Decision {
	return Decision{
		Status:              "blocked",
		RelativePath:        relativePath,
		Confidence:          "none",
		Reason:              reason,
		CandidateEpisodeIDs: candidateIDs,
		Evidence:            evidence,
		Explanation:         explanation,
	}
}

func intPointer(value int) *int {
	return &value
}
