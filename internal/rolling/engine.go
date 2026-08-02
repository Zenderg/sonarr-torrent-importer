package rolling

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zenderg/sonarr-torrent-importer/internal/mapper"
	"github.com/zenderg/sonarr-torrent-importer/internal/metainfo"
	"github.com/zenderg/sonarr-torrent-importer/internal/prowlarr"
	"github.com/zenderg/sonarr-torrent-importer/internal/qbittorrent"
	"github.com/zenderg/sonarr-torrent-importer/internal/sonarr"
	"github.com/zenderg/sonarr-torrent-importer/internal/workflow"
)

var releaseIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

const attemptHeartbeatInterval = 5 * time.Minute

type workflowEvidence interface {
	RollingEvidence(context.Context, string) (workflow.RollingEvidence, error)
}

type prowlarrAPI interface {
	Search(context.Context, string, []int, int) ([]prowlarr.Release, error)
	Download(context.Context, string) ([]byte, error)
}

type qbittorrentAPI interface {
	Login(context.Context) error
	Versions(context.Context) (qbittorrent.Versions, error)
	Torrent(context.Context, string) (qbittorrent.Torrent, error)
	Files(context.Context, string) ([]qbittorrent.File, error)
	Properties(context.Context, string) (qbittorrent.TorrentProperties, error)
	PieceHashes(context.Context, string) ([]string, error)
	AddTorrent(context.Context, []byte, qbittorrent.AddTorrentOptions) error
	Stop(context.Context, string) error
	Start(context.Context, string) error
	SetForceStart(context.Context, string, bool) error
	Recheck(context.Context, string) error
	DeleteTorrentRecord(context.Context, string) error
	RenameFile(context.Context, string, string, string) error
}

type sonarrAPI interface {
	Episodes(context.Context, int, int) ([]sonarr.Episode, error)
	EpisodeFiles(context.Context, []int) ([]sonarr.EpisodeFile, error)
	ManualImportCandidates(context.Context, string, string) ([]sonarr.ManualImportCandidate, error)
	Reprocess(context.Context, []sonarr.ManualImportReprocess) ([]sonarr.ManualImportReprocess, error)
	StartManualImport(context.Context, []sonarr.ManualImportFile) (sonarr.Command, error)
	StartManualImportWithMode(context.Context, []sonarr.ManualImportFile, string) (sonarr.Command, error)
	Command(context.Context, int) (sonarr.Command, error)
	History(context.Context, string) ([]sonarr.HistoryRecord, error)
	RecentImportHistory(context.Context) ([]sonarr.HistoryRecord, error)
	ImportHistorySince(context.Context, time.Time) ([]sonarr.HistoryRecord, error)
}

type Engine struct {
	workflow         workflowEvidence
	prowlarr         prowlarrAPI
	qbittorrent      qbittorrentAPI
	sonarr           sonarrAPI
	store            *store
	remoteMediaRoot  string
	sonarrMediaRoot  string
	localMediaRoot   string
	pollInterval     time.Duration
	revisionInterval time.Duration
	commandTimeout   time.Duration
}

func NewEngine(workflowEngine workflowEvidence, prowlarrClient prowlarrAPI, qbitClient qbittorrentAPI, sonarrClient sonarrAPI, dataRoot, remoteMediaRoot, sonarrMediaRoot, localMediaRoot string, pollInterval, revisionInterval, commandTimeout time.Duration) (*Engine, error) {
	state, err := newStore(dataRoot)
	if err != nil {
		return nil, err
	}
	remoteMediaRoot = strings.TrimRight(path.Clean(remoteMediaRoot), "/")
	if remoteMediaRoot == "." || !strings.HasPrefix(remoteMediaRoot, "/") {
		return nil, fmt.Errorf("qBittorrent media root must be an absolute POSIX path")
	}
	sonarrMediaRoot = strings.TrimRight(path.Clean(sonarrMediaRoot), "/")
	if sonarrMediaRoot == "." || !strings.HasPrefix(sonarrMediaRoot, "/") {
		return nil, fmt.Errorf("Sonarr media root must be an absolute POSIX path")
	}
	engine := &Engine{
		workflow: workflowEngine, prowlarr: prowlarrClient, qbittorrent: qbitClient, sonarr: sonarrClient,
		store: state, remoteMediaRoot: remoteMediaRoot, sonarrMediaRoot: sonarrMediaRoot, localMediaRoot: localMediaRoot,
		pollInterval: pollInterval, revisionInterval: revisionInterval, commandTimeout: commandTimeout,
	}
	if _, err := engine.safeLocalPath(remoteMediaRoot); err != nil {
		return nil, fmt.Errorf("validate rolling media roots: %w", err)
	}
	return engine, nil
}

func (e *Engine) sonarrPath(remote string) (string, error) {
	remote = path.Clean(remote)
	relative := ""
	if remote != e.remoteMediaRoot {
		prefix := e.remoteMediaRoot + "/"
		if !strings.HasPrefix(remote, prefix) {
			return "", fmt.Errorf("qBittorrent path %q is outside QBITTORRENT_MEDIA_ROOT", remote)
		}
		relative = strings.TrimPrefix(remote, prefix)
	}
	translated := path.Join(e.sonarrMediaRoot, relative)
	if translated != e.sonarrMediaRoot && !strings.HasPrefix(translated, e.sonarrMediaRoot+"/") {
		return "", fmt.Errorf("translated Sonarr path escapes SONARR_MEDIA_ROOT")
	}
	return translated, nil
}

func (e *Engine) List(context.Context) ([]Release, error) {
	return e.store.list()
}

func (e *Engine) Get(_ context.Context, id string) (Release, bool, error) {
	if !releaseIDPattern.MatchString(id) {
		return Release{}, false, fmt.Errorf("invalid release id")
	}
	return e.store.load(id)
}

func (e *Engine) Enroll(ctx context.Context, request EnrollmentRequest) (Release, error) {
	request.ReleaseID = strings.ToLower(strings.TrimSpace(request.ReleaseID))
	request.DownloadID = strings.ToLower(strings.TrimSpace(request.DownloadID))
	request.ConfirmDownloadID = strings.ToLower(strings.TrimSpace(request.ConfirmDownloadID))
	request.GUID = strings.TrimSpace(request.GUID)
	request.Query = strings.TrimSpace(request.Query)
	if !releaseIDPattern.MatchString(request.ReleaseID) {
		return Release{}, fmt.Errorf("releaseId must match %s", releaseIDPattern.String())
	}
	if request.DownloadID == "" || request.DownloadID != request.ConfirmDownloadID {
		return Release{}, fmt.Errorf("confirmDownloadId must exactly match downloadId")
	}
	if request.IndexerID <= 0 || request.GUID == "" || request.Query == "" {
		return Release{}, fmt.Errorf("indexerId, guid, and query are required")
	}

	guard, err := e.store.tryLock()
	if err != nil {
		return Release{}, err
	}
	defer guard.Close()

	if existing, found, err := e.store.load(request.ReleaseID); err != nil {
		return Release{}, err
	} else if found {
		if existing.Source.IndexerID == request.IndexerID && existing.Source.GUID == request.GUID && existing.Source.Query == request.Query && strings.EqualFold(existing.CurrentRevision.TorrentID, request.DownloadID) {
			return existing, nil
		}
		return Release{}, fmt.Errorf("releaseId %q is already enrolled with different immutable identity", request.ReleaseID)
	}
	all, err := e.store.list()
	if err != nil {
		return Release{}, err
	}
	for _, existing := range all {
		if existing.Source.IndexerID == request.IndexerID && existing.Source.GUID == request.GUID {
			return Release{}, fmt.Errorf("Prowlarr indexerId/guid is already owned by release %q", existing.ID)
		}
	}
	if err := e.qbittorrent.Login(ctx); err != nil {
		return Release{}, fmt.Errorf("authenticate with qBittorrent: %w", err)
	}
	versions, err := e.qbittorrent.Versions(ctx)
	if err != nil {
		return Release{}, fmt.Errorf("discover qBittorrent Web API version: %w", err)
	}
	if !versionAtLeast(versions.WebAPI, 2, 14) {
		return Release{}, fmt.Errorf("rolling releases require qBittorrent Web API >= 2.14.0; server reported %q", versions.WebAPI)
	}
	if !versionAtLeast(versions.Libtorrent, 2, 0) {
		return Release{}, fmt.Errorf("rolling releases require qBittorrent built with libtorrent >= 2.0; server reported %q", versions.Libtorrent)
	}

	evidence, err := e.workflow.RollingEvidence(ctx, request.DownloadID)
	if err != nil {
		return Release{}, err
	}
	selected, raw, parsed, err := e.fetch(ctx, Source{Adapter: "prowlarr-search", IndexerID: request.IndexerID, GUID: request.GUID, Query: request.Query})
	if err != nil {
		return Release{}, err
	}
	identity := torrentIdentities(parsed)
	if !strings.EqualFold(identity.TorrentID, evidence.DownloadID) {
		return Release{}, fmt.Errorf("selected Prowlarr metadata has qBittorrent torrent ID %s, expected completed downloadId %s", identity.TorrentID, evidence.DownloadID)
	}
	if err := e.verifyQbitMetainfo(ctx, evidence.Torrent, parsed); err != nil {
		return Release{}, err
	}
	revision, err := e.buildCurrentRevision(ctx, evidence, parsed, raw)
	if err != nil {
		return Release{}, err
	}
	if selected.Size > 0 && selected.Size != parsed.TotalLength {
		return Release{}, fmt.Errorf("Prowlarr size %d does not match verified torrent payload %d", selected.Size, parsed.TotalLength)
	}
	if err := e.store.saveArtifact(revision.ArtifactSHA256, raw); err != nil {
		return Release{}, err
	}
	now := time.Now().UTC()
	release := Release{
		Version:  recordVersion,
		ID:       request.ReleaseID,
		Source:   Source{Adapter: "prowlarr-search", IndexerID: request.IndexerID, GUID: request.GUID, Query: request.Query},
		SeriesID: evidence.SeriesID, SeasonNumber: evidence.SeasonNumber,
		SeriesTitle: evidence.SeriesTitle, QualityName: evidence.QualityName,
		CurrentRevision: revision, Status: "current", LastCheckedAt: now,
		NextCheckAt: now.Add(e.revisionInterval), CreatedAt: now,
		UpdatedAt: now,
	}
	if err := e.store.save(release); err != nil {
		return Release{}, err
	}
	return release, nil
}

func (e *Engine) Check(ctx context.Context, id string) (CheckResult, error) {
	if !releaseIDPattern.MatchString(id) {
		return CheckResult{}, fmt.Errorf("invalid release id")
	}
	guard, err := e.store.tryLock()
	if err != nil {
		return CheckResult{}, err
	}
	defer guard.Close()
	release, found, err := e.store.load(id)
	if err != nil {
		return CheckResult{}, err
	}
	if !found {
		return CheckResult{}, fmt.Errorf("rolling release %q was not found", id)
	}
	if release.Status == "blocked" {
		release.Status = "updating"
		release.BlockedReason = ""
	}
	if release.Operation != nil {
		before := release.Operation.Phase
		if err := e.advance(ctx, &release); err != nil {
			if saveErr := e.recordAttempt(&release, err); saveErr != nil {
				return CheckResult{Release: release, Changed: release.Operation != nil && release.Operation.Phase != before, Message: err.Error()}, fmt.Errorf("%v; persist rolling attempt: %w", err, saveErr)
			}
			return CheckResult{Release: release, Changed: release.Operation != nil && release.Operation.Phase != before, Message: err.Error()}, err
		}
		if err := e.recordAttempt(&release, nil); err != nil {
			return CheckResult{Release: release, Changed: release.Operation == nil || release.Operation.Phase != before}, fmt.Errorf("persist rolling attempt: %w", err)
		}
		return CheckResult{Release: release, Changed: release.Operation == nil || release.Operation.Phase != before, Message: "rolling operation reconciled"}, nil
	}
	changed, message, err := e.discover(ctx, &release)
	if saveErr := e.recordAttempt(&release, err); saveErr != nil {
		return CheckResult{Release: release, Changed: changed, Message: message}, fmt.Errorf("%v; persist rolling attempt: %w", err, saveErr)
	}
	return CheckResult{Release: release, Changed: changed, Message: message}, err
}

func (e *Engine) recordAttempt(release *Release, attemptErr error) error {
	now := time.Now().UTC()
	previousAt := release.LastAttemptAt
	previousError := release.LastAttemptError
	nextError := ""
	if attemptErr != nil {
		nextError = attemptErr.Error()
	}
	release.LastAttemptAt = now
	release.LastAttemptError = nextError
	if previousError == nextError && !previousAt.IsZero() && now.Sub(previousAt) < attemptHeartbeatInterval {
		return nil
	}
	return e.store.save(*release)
}

func (e *Engine) Run(ctx context.Context) {
	interval := e.pollInterval
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	e.reconcileDue(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.reconcileDue(ctx)
		}
	}
}

func (e *Engine) reconcileDue(ctx context.Context) {
	releases, err := e.store.list()
	if err != nil {
		slog.Error("rolling release reconciliation cannot list durable state", "error", err)
		return
	}
	now := time.Now().UTC()
	for _, release := range releases {
		if ctx.Err() != nil {
			return
		}
		if release.Status == "blocked" || (release.Operation == nil && now.Before(release.NextCheckAt)) {
			continue
		}
		_, checkErr := e.Check(ctx, release.ID)
		if checkErr != nil && ctx.Err() == nil {
			slog.Warn("rolling release reconciliation failed", "releaseId", release.ID, "error", checkErr)
		}
	}
}

func (e *Engine) discover(ctx context.Context, release *Release) (bool, string, error) {
	_, raw, parsed, err := e.fetch(ctx, release.Source)
	now := time.Now().UTC()
	release.LastCheckedAt = now
	release.NextCheckAt = now.Add(e.revisionInterval)
	if err != nil {
		release.Status = "source_error"
		release.BlockedReason = err.Error()
		_ = e.saveRelease(release)
		return false, "source check failed", err
	}
	identity := torrentIdentities(parsed)
	if strings.EqualFold(identity.TorrentID, release.CurrentRevision.TorrentID) {
		release.Status = "current"
		release.BlockedReason = ""
		if err := e.saveRelease(release); err != nil {
			return false, "", err
		}
		return false, "source still points to the current revision", nil
	}
	if release.CandidateRevision != nil && strings.EqualFold(identity.TorrentID, release.CandidateRevision.TorrentID) {
		return false, "candidate revision is already staged", nil
	}
	candidate, err := e.buildCandidate(ctx, *release, parsed, raw)
	if err != nil {
		release.Status = "blocked"
		release.BlockedReason = err.Error()
		_ = e.saveRelease(release)
		return false, "candidate revision is incompatible", err
	}
	if err := e.store.saveArtifact(candidate.ArtifactSHA256, raw); err != nil {
		return false, "", err
	}
	history, err := e.sonarr.RecentImportHistory(ctx)
	if err != nil {
		return false, "", fmt.Errorf("capture Sonarr history baseline: %w", err)
	}
	baseline := make([]int, 0, len(history))
	for _, record := range history {
		baseline = append(baseline, record.ID)
	}
	sort.Ints(baseline)
	operation := Operation{
		ID: release.ID + ":" + candidate.TorrentID, Phase: "prepared",
		OldTorrentID: release.CurrentRevision.TorrentID, NewTorrentID: candidate.TorrentID,
		HistoryBaseline: baseline, StartedAt: now, UpdatedAt: now,
	}
	operation.PlanToken, err = planToken(*release, candidate)
	if err != nil {
		return false, "", err
	}
	release.CandidateRevision = &candidate
	release.Operation = &operation
	release.Status = "updating"
	release.BlockedReason = ""
	if err := e.saveRelease(release); err != nil {
		return false, "", err
	}
	if err := e.advance(ctx, release); err != nil {
		return true, "new revision staged", err
	}
	return true, "new revision staged and execution started", nil
}

func (e *Engine) fetch(ctx context.Context, source Source) (prowlarr.Release, []byte, metainfo.MetaInfo, error) {
	releases, err := e.prowlarr.Search(ctx, source.Query, []int{source.IndexerID}, 100)
	if err != nil {
		return prowlarr.Release{}, nil, metainfo.MetaInfo{}, err
	}
	matched := make([]prowlarr.Release, 0, 1)
	for _, candidate := range releases {
		if candidate.IndexerID == source.IndexerID && candidate.GUID == source.GUID && strings.EqualFold(candidate.Protocol, "torrent") {
			matched = append(matched, candidate)
		}
	}
	if len(matched) != 1 {
		return prowlarr.Release{}, nil, metainfo.MetaInfo{}, fmt.Errorf("Prowlarr selector matched %d torrent releases; expected exactly one", len(matched))
	}
	if matched[0].DownloadURL == "" {
		return prowlarr.Release{}, nil, metainfo.MetaInfo{}, fmt.Errorf("selected Prowlarr release has no download URL")
	}
	raw, err := e.prowlarr.Download(ctx, matched[0].DownloadURL)
	if err != nil {
		return prowlarr.Release{}, nil, metainfo.MetaInfo{}, err
	}
	parsed, err := metainfo.Parse(raw)
	if err != nil {
		return prowlarr.Release{}, nil, metainfo.MetaInfo{}, fmt.Errorf("validate selected torrent metadata: %w", err)
	}
	return matched[0], raw, parsed, nil
}

func (e *Engine) verifyQbitMetainfo(ctx context.Context, torrent qbittorrent.Torrent, parsed metainfo.MetaInfo) error {
	properties, err := e.qbittorrent.Properties(ctx, torrent.Hash)
	if err != nil {
		return fmt.Errorf("read qBittorrent torrent properties: %w", err)
	}
	if properties.PieceSize != parsed.PieceLength || properties.PiecesNum <= 0 || properties.PiecesHave != properties.PiecesNum {
		return fmt.Errorf("qBittorrent piece topology does not match completed source metadata")
	}
	identity := torrentIdentities(parsed)
	if !torrentMatchesIdentity(torrent, identity) {
		return fmt.Errorf("qBittorrent identity does not match completed source metadata")
	}
	if parsed.V2Only {
		return nil
	}
	if properties.PiecesNum != len(parsed.PieceHashes) {
		return fmt.Errorf("qBittorrent v1 piece count does not match completed source metadata")
	}
	hashes, err := e.qbittorrent.PieceHashes(ctx, torrent.Hash)
	if err != nil {
		return fmt.Errorf("read qBittorrent piece hashes: %w", err)
	}
	if len(hashes) != len(parsed.PieceHashes) {
		return fmt.Errorf("qBittorrent returned %d piece hashes, metadata has %d", len(hashes), len(parsed.PieceHashes))
	}
	for index := range hashes {
		if !strings.EqualFold(hashes[index], hex.EncodeToString(parsed.PieceHashes[index][:])) {
			return fmt.Errorf("qBittorrent piece hash %d does not match enrolled metadata", index)
		}
	}
	return nil
}

func (e *Engine) buildCurrentRevision(ctx context.Context, evidence workflow.RollingEvidence, parsed metainfo.MetaInfo, raw []byte) (Revision, error) {
	manifestByIndex := make(map[int]workflow.RollingEvidenceFile, len(evidence.Files))
	for _, file := range evidence.Files {
		manifestByIndex[file.Index] = file
	}
	qbitFiles, err := e.qbittorrent.Files(ctx, evidence.Torrent.Hash)
	if err != nil {
		return Revision{}, fmt.Errorf("read current qBittorrent manifest: %w", err)
	}
	qbitByIndex := make(map[int]qbittorrent.File, len(qbitFiles))
	for _, file := range qbitFiles {
		qbitByIndex[file.Index] = file
	}
	files := make([]RevisionFile, 0, len(parsed.Files))
	for _, metaFile := range parsed.Files {
		qbitFile, ok := qbitByIndex[metaFile.Index]
		if !ok || qbitFile.Size != metaFile.Length || qbitFile.Progress != 1 || qbitFile.Priority == 0 {
			return Revision{}, fmt.Errorf("qBittorrent manifest file %d does not match enrolled metadata", metaFile.Index)
		}
		file := RevisionFile{Index: metaFile.Index, RawPath: metaFile.Path, CurrentPath: qbitFile.Name, Size: metaFile.Length}
		if owned, ok := manifestByIndex[metaFile.Index]; ok {
			if qbitFile.Name != owned.CanonicalPath || owned.Size != metaFile.Length {
				return Revision{}, fmt.Errorf("owned file %d does not match canonical completed operation", metaFile.Index)
			}
			file.CanonicalPath = owned.CanonicalPath
			file.EpisodeID = owned.EpisodeID
			file.EpisodeFileID = owned.EpisodeFileID
			file.LibraryPath = owned.ImportedPath
			digest, err := e.digestRemoteFile(ctx, path.Join(evidence.Torrent.SavePath, qbitFile.Name), metaFile.Length)
			if err != nil {
				return Revision{}, fmt.Errorf("hash enrolled file %q: %w", qbitFile.Name, err)
			}
			file.ContentSHA256 = digest
		}
		files = append(files, file)
	}
	artifact := sha256.Sum256(raw)
	identity := torrentIdentities(parsed)
	sonarrSavePath, err := e.sonarrPath(evidence.Torrent.SavePath)
	if err != nil {
		return Revision{}, err
	}
	return Revision{
		ID: revisionID(parsed, identity), TorrentID: identity.TorrentID,
		InfoHashV1: identity.InfoHashV1, InfoHashV2: identity.InfoHashV2,
		ArtifactSHA256: hex.EncodeToString(artifact[:]), Name: parsed.Name,
		PieceLength: parsed.PieceLength, TotalLength: parsed.TotalLength,
		SavePath: evidence.Torrent.SavePath, SonarrSavePath: sonarrSavePath,
		Category: evidence.Torrent.Category, Tags: evidence.Torrent.Tags,
		AddedOn: evidence.Torrent.AddedOn,
		Files:   files, ObservedAt: time.Now().UTC(),
	}, nil
}

func (e *Engine) buildCandidate(ctx context.Context, release Release, parsed metainfo.MetaInfo, raw []byte) (Revision, error) {
	if !parsed.MultiFile {
		return Revision{}, fmt.Errorf("rolling revisions require a multi-file torrent root")
	}
	episodes, err := e.sonarr.Episodes(ctx, release.SeriesID, release.SeasonNumber)
	if err != nil {
		return Revision{}, fmt.Errorf("read Sonarr episodes: %w", err)
	}
	episodeByID := make(map[int]sonarr.Episode, len(episodes))
	mapperEpisodes := make([]mapper.Episode, 0, len(episodes))
	for _, episode := range episodes {
		episodeByID[episode.ID] = episode
		mapperEpisodes = append(mapperEpisodes, mapper.Episode{ID: episode.ID, SeriesID: episode.SeriesID, SeasonNumber: episode.SeasonNumber, EpisodeNumber: episode.EpisodeNumber, Title: episode.Title})
	}
	media := make([]mapper.File, 0)
	mediaIndexes := make([]int, 0)
	for index, file := range parsed.Files {
		if strings.EqualFold(path.Ext(file.Path), ".mkv") {
			media = append(media, mapper.File{RelativePath: file.Path, Size: file.Length})
			mediaIndexes = append(mediaIndexes, index)
		}
	}
	if len(media) == 0 {
		return Revision{}, fmt.Errorf("candidate contains no supported media files")
	}
	identity := torrentIdentities(parsed)
	mapping := mapper.Map(media, mapper.Context{SeriesID: release.SeriesID, SeasonNumber: release.SeasonNumber, DownloadID: identity.TorrentID, Source: "rollingRelease"}, mapperEpisodes)
	if !mapping.CanExecute {
		for _, decision := range mapping.Decisions {
			if decision.Status == "blocked" {
				return Revision{}, fmt.Errorf("map %q: %s", decision.RelativePath, decision.Reason)
			}
		}
		return Revision{}, fmt.Errorf("candidate media mapping is incomplete")
	}
	currentByEpisode := make(map[int]RevisionFile)
	for _, current := range release.CurrentRevision.Files {
		if current.EpisodeID > 0 {
			currentByEpisode[current.EpisodeID] = current
		}
	}
	files := make([]RevisionFile, len(parsed.Files))
	for index, metaFile := range parsed.Files {
		files[index] = RevisionFile{Index: metaFile.Index, RawPath: metaFile.Path, CurrentPath: metaFile.Path, Size: metaFile.Length}
	}
	newEpisodes := 0
	seenCurrent := make(map[int]struct{})
	for mapIndex, decision := range mapping.Decisions {
		fileIndex := mediaIndexes[mapIndex]
		episode := episodeByID[decision.EpisodeIDs[0]]
		canonical, err := workflow.CanonicalTorrentPath(files[fileIndex].RawPath, release.SeriesTitle, release.QualityName, episode.EpisodeNumber, episode.SeasonNumber)
		if err != nil {
			return Revision{}, fmt.Errorf("canonicalize %q: %w", files[fileIndex].RawPath, err)
		}
		files[fileIndex].CanonicalPath = canonical
		files[fileIndex].EpisodeID = episode.ID
		files[fileIndex].EpisodeNumber = episode.EpisodeNumber
		if current, owned := currentByEpisode[episode.ID]; owned {
			seenCurrent[episode.ID] = struct{}{}
			if current.Size != files[fileIndex].Size {
				return Revision{}, fmt.Errorf("candidate changes size of already imported episode %d", episode.ID)
			}
			if !episode.HasFile || episode.EpisodeFileID != current.EpisodeFileID || episode.EpisodeFile == nil || episode.EpisodeFile.ID != current.EpisodeFileID || episode.EpisodeFile.Size != current.Size || !samePath(episode.EpisodeFile.Path, current.LibraryPath) {
				return Revision{}, fmt.Errorf("Sonarr ownership receipt for existing episode %d is stale", episode.ID)
			}
			files[fileIndex].EpisodeFileID = current.EpisodeFileID
			files[fileIndex].LibraryPath = current.LibraryPath
			files[fileIndex].ContentSHA256 = current.ContentSHA256
		} else {
			if episode.HasFile {
				return Revision{}, fmt.Errorf("episode %d already has a Sonarr file not owned by this rolling release", episode.ID)
			}
			files[fileIndex].ImportNeeded = true
			newEpisodes++
		}
	}
	for episodeID := range currentByEpisode {
		if _, ok := seenCurrent[episodeID]; !ok {
			return Revision{}, fmt.Errorf("candidate removes already imported episode %d", episodeID)
		}
	}
	if newEpisodes == 0 {
		return Revision{}, fmt.Errorf("candidate has a new info hash but adds no missing Sonarr episodes")
	}
	artifact := sha256.Sum256(raw)
	savePath := path.Join(e.remoteMediaRoot, ".sonarr-torrent-importer", release.ID, identity.TorrentID)
	if _, err := e.safeLocalPath(savePath); err != nil {
		return Revision{}, err
	}
	sonarrSavePath, err := e.sonarrPath(savePath)
	if err != nil {
		return Revision{}, err
	}
	return Revision{
		ID: revisionID(parsed, identity), TorrentID: identity.TorrentID,
		InfoHashV1: identity.InfoHashV1, InfoHashV2: identity.InfoHashV2,
		ArtifactSHA256: hex.EncodeToString(artifact[:]), Name: parsed.Name,
		PieceLength: parsed.PieceLength, TotalLength: parsed.TotalLength,
		SavePath: savePath, SonarrSavePath: sonarrSavePath, Category: release.CurrentRevision.Category,
		Tags: managedTags(release.CurrentRevision.Tags), Files: files, ObservedAt: time.Now().UTC(),
	}, nil
}

func planToken(release Release, revision Revision) (string, error) {
	payload := struct {
		ReleaseID string   `json:"releaseId"`
		OldID     string   `json:"oldId"`
		Source    Source   `json:"source"`
		Revision  Revision `json:"revision"`
	}{release.ID, release.CurrentRevision.TorrentID, release.Source, revision}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func managedTags(existing string) string {
	for _, tag := range strings.Split(existing, ",") {
		if strings.TrimSpace(tag) == "sonarr-torrent-importer" {
			return existing
		}
	}
	if strings.TrimSpace(existing) == "" {
		return "sonarr-torrent-importer"
	}
	return existing + ",sonarr-torrent-importer"
}

func samePath(left, right string) bool {
	normalize := func(value string) string {
		return strings.TrimRight(strings.ReplaceAll(value, `\`, "/"), "/")
	}
	return normalize(left) == normalize(right)
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "was not found")
}

func (e *Engine) torrentIfExists(ctx context.Context, hash string) (qbittorrent.Torrent, bool, error) {
	torrent, err := e.qbittorrent.Torrent(ctx, hash)
	if isNotFound(err) {
		return qbittorrent.Torrent{}, false, nil
	}
	return torrent, err == nil, err
}

func blocked(release *Release, reason string) error {
	release.Status = "blocked"
	release.BlockedReason = reason
	return errors.New(reason)
}

func versionAtLeast(raw string, requiredMajor, requiredMinor int) bool {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(raw), "v"), ".")
	if len(parts) < 2 {
		return false
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil {
		return false
	}
	return major > requiredMajor || (major == requiredMajor && minor >= requiredMinor)
}

type torrentIdentity struct {
	TorrentID  string
	InfoHashV1 string
	InfoHashV2 string
}

func torrentIdentities(parsed metainfo.MetaInfo) torrentIdentity {
	identity := torrentIdentity{}
	if !parsed.V2Only {
		identity.InfoHashV1 = hex.EncodeToString(parsed.RawInfoSHA1[:])
	}
	if parsed.V2Only || parsed.Hybrid {
		identity.InfoHashV2 = hex.EncodeToString(parsed.RawInfoSHA256[:])
		identity.TorrentID = identity.InfoHashV2[:40]
	} else {
		identity.TorrentID = identity.InfoHashV1
	}
	return identity
}

func torrentMatchesIdentity(torrent qbittorrent.Torrent, identity torrentIdentity) bool {
	return strings.EqualFold(torrent.Hash, identity.TorrentID) &&
		strings.EqualFold(torrent.InfoHashV1, identity.InfoHashV1) &&
		strings.EqualFold(torrent.InfoHashV2, identity.InfoHashV2)
}

func revisionMatchesTorrent(revision Revision, torrent qbittorrent.Torrent) bool {
	return torrentMatchesIdentity(torrent, torrentIdentity{
		TorrentID: revision.TorrentID, InfoHashV1: revision.InfoHashV1, InfoHashV2: revision.InfoHashV2,
	})
}

func revisionID(parsed metainfo.MetaInfo, identity torrentIdentity) string {
	if parsed.V2Only {
		return "btmh:1220" + identity.InfoHashV2
	}
	return "btih:" + identity.InfoHashV1
}
