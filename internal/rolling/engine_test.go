package rolling

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zenderg/sonarr-torrent-importer/internal/metainfo"
	"github.com/zenderg/sonarr-torrent-importer/internal/qbittorrent"
	"github.com/zenderg/sonarr-torrent-importer/internal/sonarr"
)

type candidateSonarr struct{ episodes []sonarr.Episode }

type candidateQbit struct {
	qbittorrentAPI
	torrent     qbittorrent.Torrent
	files       []qbittorrent.File
	rechecks    int
	forceStarts int
}

func (q *candidateQbit) Recheck(context.Context, string) error {
	q.rechecks++
	return nil
}

func (q *candidateQbit) Torrent(context.Context, string) (qbittorrent.Torrent, error) {
	return q.torrent, nil
}

func (q *candidateQbit) Files(context.Context, string) ([]qbittorrent.File, error) {
	return q.files, nil
}

func (q *candidateQbit) SetForceStart(_ context.Context, _ string, enabled bool) error {
	q.forceStarts++
	q.torrent.ForceStart = enabled
	return nil
}

func (s *candidateSonarr) Episodes(context.Context, int, int) ([]sonarr.Episode, error) {
	return s.episodes, nil
}
func (s *candidateSonarr) EpisodeFiles(context.Context, []int) ([]sonarr.EpisodeFile, error) {
	return nil, nil
}
func (s *candidateSonarr) ManualImportCandidates(context.Context, string, string) ([]sonarr.ManualImportCandidate, error) {
	return nil, nil
}
func (s *candidateSonarr) Reprocess(context.Context, []sonarr.ManualImportReprocess) ([]sonarr.ManualImportReprocess, error) {
	return nil, nil
}
func (s *candidateSonarr) StartManualImport(context.Context, []sonarr.ManualImportFile) (sonarr.Command, error) {
	return sonarr.Command{}, nil
}
func (s *candidateSonarr) StartManualImportWithMode(context.Context, []sonarr.ManualImportFile, string) (sonarr.Command, error) {
	return sonarr.Command{}, nil
}
func (s *candidateSonarr) Command(context.Context, int) (sonarr.Command, error) {
	return sonarr.Command{}, nil
}
func (s *candidateSonarr) History(context.Context, string) ([]sonarr.HistoryRecord, error) {
	return nil, nil
}
func (s *candidateSonarr) RecentImportHistory(context.Context) ([]sonarr.HistoryRecord, error) {
	return nil, nil
}
func (s *candidateSonarr) ImportHistorySince(context.Context, time.Time) ([]sonarr.HistoryRecord, error) {
	return nil, nil
}

func TestBuildCandidateImportsOnlyNewOwnedEpisodes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	engine := &Engine{
		sonarr: &candidateSonarr{episodes: []sonarr.Episode{
			{ID: 101, SeriesID: 7, SeasonNumber: 1, EpisodeNumber: 1, HasFile: true, EpisodeFileID: 501, EpisodeFile: &sonarr.EpisodeFile{ID: 501, SeriesID: 7, SeasonNumber: 1, Size: 10, Path: "/library/episode-1.mkv"}},
			{ID: 102, SeriesID: 7, SeasonNumber: 1, EpisodeNumber: 2},
		}},
		remoteMediaRoot: "/downloads", sonarrMediaRoot: "/sonarr-downloads", localMediaRoot: root,
	}
	release := Release{
		ID: "show-s01", SeriesID: 7, SeasonNumber: 1, SeriesTitle: "A Show", QualityName: "WEBDL-720p",
		CurrentRevision: Revision{TorrentID: "1111111111111111111111111111111111111111", Files: []RevisionFile{{EpisodeID: 101, EpisodeFileID: 501, Size: 10, LibraryPath: "/library/episode-1.mkv", ContentSHA256: "owned"}}},
	}
	var hash [20]byte
	hash[0] = 2
	parsed := metainfo.MetaInfo{
		Name: "A.Show.S01", MultiFile: true, RawInfoSHA1: hash, PieceLength: 16, TotalLength: 30,
		Files: []metainfo.File{
			{Index: 0, Path: "A.Show.S01/[01].mkv", Length: 10},
			{Index: 1, Path: "A.Show.S01/[02].mkv", Length: 20, Offset: 10},
		},
	}
	candidate, err := engine.buildCandidate(context.Background(), release, parsed, []byte("torrent"))
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Files[0].ImportNeeded || !candidate.Files[1].ImportNeeded {
		t.Fatalf("unexpected import policy: %#v", candidate.Files)
	}
	if candidate.Files[0].CanonicalPath != "A.Show.S01/A.Show.S01E01.WEBDL-720p.mkv" || candidate.Files[1].CanonicalPath != "A.Show.S01/A.Show.S01E02.WEBDL-720p.mkv" {
		t.Fatalf("unexpected canonical paths: %#v", candidate.Files)
	}
	if candidate.SavePath != "/downloads/.sonarr-torrent-importer/show-s01/0200000000000000000000000000000000000000" {
		t.Fatalf("unexpected isolated save path %q", candidate.SavePath)
	}
	if candidate.SonarrSavePath != "/sonarr-downloads/.sonarr-torrent-importer/show-s01/0200000000000000000000000000000000000000" {
		t.Fatalf("unexpected Sonarr save path %q", candidate.SonarrSavePath)
	}
}

func TestBuildCandidateBlocksChangedOwnedEpisode(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	engine := &Engine{
		sonarr:          &candidateSonarr{episodes: []sonarr.Episode{{ID: 101, SeriesID: 7, SeasonNumber: 1, EpisodeNumber: 1, HasFile: true, EpisodeFileID: 501, EpisodeFile: &sonarr.EpisodeFile{ID: 501, SeriesID: 7, SeasonNumber: 1, Size: 10, Path: "/library/episode-1.mkv"}}}},
		remoteMediaRoot: "/downloads", sonarrMediaRoot: "/downloads", localMediaRoot: root,
	}
	release := Release{ID: "show-s01", SeriesID: 7, SeasonNumber: 1, SeriesTitle: "A Show", QualityName: "WEBDL-720p", CurrentRevision: Revision{Files: []RevisionFile{{EpisodeID: 101, EpisodeFileID: 501, Size: 10, LibraryPath: "/library/episode-1.mkv"}}}}
	parsed := metainfo.MetaInfo{Name: "A.Show.S01", MultiFile: true, PieceLength: 16, TotalLength: 11, Files: []metainfo.File{{Index: 0, Path: "A.Show.S01/[01].mkv", Length: 11}}}
	if _, err := engine.buildCandidate(context.Background(), release, parsed, []byte("torrent")); err == nil {
		t.Fatal("changed owned episode size was accepted")
	}
}

func TestCopyOwnedFileCreatesIndependentVerifiedCopy(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "old", "Show", "episode.mkv")
	if err := os.MkdirAll(filepath.Dir(source), 0o750); err != nil {
		t.Fatal(err)
	}
	content := []byte("immutable episode bytes")
	if err := os.WriteFile(source, content, 0o640); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{remoteMediaRoot: "/downloads", localMediaRoot: root, revisionInterval: time.Hour}
	digest, err := engine.digestRemoteFile(context.Background(), "/downloads/old/Show/episode.mkv", int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	release := Release{
		CurrentRevision:   Revision{SavePath: "/downloads/old", Files: []RevisionFile{{EpisodeID: 1, CurrentPath: "Show/episode.mkv", Size: int64(len(content)), ContentSHA256: digest}}},
		CandidateRevision: &Revision{SavePath: "/downloads/new", Files: []RevisionFile{{EpisodeID: 1, RawPath: "Show/[01].mkv", Size: int64(len(content)), ContentSHA256: digest}}},
	}
	if err := engine.copyOwnedFile(context.Background(), &release, 0); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "new", "Show", "[01].mkv")
	sourceInfo, _ := os.Stat(source)
	targetInfo, _ := os.Stat(target)
	if os.SameFile(sourceInfo, targetInfo) {
		t.Fatal("reuse created a hardlink instead of an independent copy")
	}
	if err := os.WriteFile(target, []byte("mutated staging bytes!!"), 0o640); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(source)
	if err != nil || string(actual) != string(content) {
		t.Fatalf("source changed after staging mutation: %q, %v", actual, err)
	}
	if err := os.WriteFile(source, []byte("changed current bytes!!!"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := engine.verifyRetainedOldFiles(context.Background(), release.CurrentRevision); err == nil {
		t.Fatal("changed retained old content passed its durable digest receipt")
	}
}

func TestCancelledRollingCopyRemovesInterruptedTemporaryFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source.mkv")
	target := filepath.Join(root, "staging", "episode.mkv")
	if err := os.WriteFile(source, make([]byte, 2*1024*1024), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(target), ".rolling-copy-orphan.tmp"), []byte("orphan"), 0o640); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := copyRegularFileAtomic(ctx, root, source, target, 2*1024*1024)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copy error = %v, want context cancellation", err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(target), ".rolling-copy-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("interrupted rolling copy files survived cancellation: %v", matches)
	}
}

func TestAcceptedManualImportBlocksAtDurableDeadline(t *testing.T) {
	t.Parallel()
	engine := &Engine{
		sonarr: &candidateSonarr{}, commandTimeout: time.Minute,
		remoteMediaRoot: "/downloads", sonarrMediaRoot: "/downloads",
	}
	release := Release{
		SeriesID: 1, SeasonNumber: 1, Status: "updating",
		CurrentRevision:   Revision{SavePath: "/downloads", SonarrSavePath: "/downloads"},
		CandidateRevision: &Revision{SavePath: "/downloads", SonarrSavePath: "/downloads", Files: []RevisionFile{{EpisodeID: 10, ImportNeeded: true}}},
		Operation: &Operation{
			Phase: "command_accepted", CommandID: 99,
			CommandAcceptedAt: time.Now().Add(-2 * time.Minute),
			StartedAt:         time.Now().Add(-2 * time.Minute),
		},
	}
	_, _, err := engine.advanceOne(context.Background(), &release)
	if err == nil || release.Status != "blocked" {
		t.Fatalf("expired ManualImport state = %q, error = %v", release.Status, err)
	}
}

func TestRollingPathsRemainCaseSensitive(t *testing.T) {
	t.Parallel()
	if samePath("/media/Show/Episode.mkv", "/media/show/Episode.mkv") {
		t.Fatal("case-distinct POSIX paths were treated as equal")
	}
	if normalizePath("/media/Show/Episode.mkv") == normalizePath("/media/show/Episode.mkv") {
		t.Fatal("case-distinct candidate paths collapsed to one key")
	}
}

func TestFastPostImportRecheckPersistsObservablePhase(t *testing.T) {
	t.Parallel()
	state, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	qbit := &candidateQbit{}
	engine := &Engine{
		qbittorrent: qbit, store: state,
		remoteMediaRoot: "/downloads", sonarrMediaRoot: "/downloads",
	}
	release := Release{
		ID: "show-s01", Status: "updating",
		CurrentRevision: Revision{SavePath: "/downloads", SonarrSavePath: "/downloads"},
		CandidateRevision: &Revision{
			TorrentID: "1111111111111111111111111111111111111111",
			SavePath:  "/downloads", SonarrSavePath: "/downloads",
		},
		Operation: &Operation{Phase: "post_import_stopped"},
	}
	progressed, waiting, err := engine.advanceOne(context.Background(), &release)
	if err != nil {
		t.Fatal(err)
	}
	if !progressed || !waiting || release.Operation.Phase != "post_import_rechecking" || qbit.rechecks != 1 {
		t.Fatalf("unexpected recheck transition: progressed=%t waiting=%t phase=%q calls=%d", progressed, waiting, release.Operation.Phase, qbit.rechecks)
	}
}

func TestTorrentIdentitiesUseQbittorrentOperationalHash(t *testing.T) {
	t.Parallel()
	var v1 [20]byte
	var v2 [32]byte
	for index := range v1 {
		v1[index] = byte(index + 1)
	}
	for index := range v2 {
		v2[index] = byte(index + 33)
	}
	v1Hex := hex.EncodeToString(v1[:])
	v2Hex := hex.EncodeToString(v2[:])

	tests := []struct {
		name       string
		parsed     metainfo.MetaInfo
		want       torrentIdentity
		revisionID string
	}{
		{
			name: "v1 only", parsed: metainfo.MetaInfo{RawInfoSHA1: v1},
			want: torrentIdentity{TorrentID: v1Hex, InfoHashV1: v1Hex}, revisionID: "btih:" + v1Hex,
		},
		{
			name: "hybrid", parsed: metainfo.MetaInfo{RawInfoSHA1: v1, RawInfoSHA256: v2, Hybrid: true},
			want: torrentIdentity{TorrentID: v2Hex[:40], InfoHashV1: v1Hex, InfoHashV2: v2Hex}, revisionID: "btih:" + v1Hex,
		},
		{
			name: "pure v2", parsed: metainfo.MetaInfo{RawInfoSHA256: v2, V2Only: true},
			want: torrentIdentity{TorrentID: v2Hex[:40], InfoHashV2: v2Hex}, revisionID: "btmh:1220" + v2Hex,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := torrentIdentities(test.parsed)
			if got != test.want {
				t.Fatalf("identity = %#v, want %#v", got, test.want)
			}
			if gotID := revisionID(test.parsed, got); gotID != test.revisionID {
				t.Fatalf("revision ID = %q, want %q", gotID, test.revisionID)
			}
		})
	}
}

func TestTorrentIdentityRequiresFullVersionedHashes(t *testing.T) {
	t.Parallel()
	identity := torrentIdentity{
		TorrentID:  "2222222222222222222222222222222222222222",
		InfoHashV1: "1111111111111111111111111111111111111111",
		InfoHashV2: "2222222222222222222222222222222222222222333333333333333333333333",
	}
	valid := qbittorrent.Torrent{Hash: identity.TorrentID, InfoHashV1: identity.InfoHashV1, InfoHashV2: identity.InfoHashV2}
	if !torrentMatchesIdentity(valid, identity) {
		t.Fatal("matching hybrid identity was rejected")
	}
	wrongOperationalID := valid
	wrongOperationalID.Hash = identity.InfoHashV1
	if torrentMatchesIdentity(wrongOperationalID, identity) {
		t.Fatal("hybrid v1 info-hash was accepted as qBittorrent operational ID")
	}
	wrongFullV2 := valid
	wrongFullV2.InfoHashV2 = identity.InfoHashV2[:63] + "4"
	if torrentMatchesIdentity(wrongFullV2, identity) {
		t.Fatal("mismatched full v2 info-hash was accepted")
	}
}

func TestQueuedCandidateIsDurablyForceStarted(t *testing.T) {
	t.Parallel()
	const torrentID = "1111111111111111111111111111111111111111"
	qbit := &candidateQbit{torrent: qbittorrent.Torrent{
		Hash: torrentID, InfoHashV1: torrentID, Name: "A.Show.S01", SavePath: "/downloads/stage",
		State: "queuedUP", Progress: 1,
	}}
	engine := &Engine{
		qbittorrent: qbit, commandTimeout: time.Minute,
		remoteMediaRoot: "/downloads", sonarrMediaRoot: "/downloads",
	}
	release := Release{
		Status:          "updating",
		CurrentRevision: Revision{SavePath: "/downloads/current", SonarrSavePath: "/downloads/current"},
		CandidateRevision: &Revision{
			TorrentID: torrentID, InfoHashV1: torrentID, Name: "A.Show.S01",
			SavePath: "/downloads/stage", SonarrSavePath: "/downloads/stage",
		},
		Operation: &Operation{Phase: "canonical_force_start_submitting", UpdatedAt: time.Now()},
	}
	progressed, waiting, err := engine.advanceOne(context.Background(), &release)
	if err != nil {
		t.Fatal(err)
	}
	if progressed || !waiting || qbit.forceStarts != 1 || !qbit.torrent.ForceStart {
		t.Fatalf("force-start reconciliation = progressed %t, waiting %t, calls %d, observed %t", progressed, waiting, qbit.forceStarts, qbit.torrent.ForceStart)
	}
	qbit.torrent.State = "forcedUP"
	progressed, waiting, err = engine.advanceOne(context.Background(), &release)
	if err != nil {
		t.Fatal(err)
	}
	if !progressed || waiting || release.Operation.Phase != "import_preparing" {
		t.Fatalf("force-start completion = progressed %t, waiting %t, phase %q", progressed, waiting, release.Operation.Phase)
	}
}

func TestAttemptHeartbeatAvoidsRewriteUntilErrorChanges(t *testing.T) {
	t.Parallel()
	state, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{store: state}
	release := Release{
		ID: "show-s01", LastAttemptAt: time.Now(),
		CurrentRevision: Revision{TorrentID: "1111111111111111111111111111111111111111"},
	}
	if err := engine.recordAttempt(&release, nil); err != nil {
		t.Fatal(err)
	}
	if _, found, err := state.load(release.ID); err != nil || found {
		t.Fatalf("unchanged heartbeat persisted unexpectedly: found=%t error=%v", found, err)
	}
	if err := engine.recordAttempt(&release, errors.New("source unavailable")); err != nil {
		t.Fatal(err)
	}
	persisted, found, err := state.load(release.ID)
	if err != nil || !found || persisted.LastAttemptError != "source unavailable" {
		t.Fatalf("changed attempt error was not persisted: found=%t release=%+v error=%v", found, persisted, err)
	}
}

type rejectingAddQbit struct{ qbittorrentAPI }

func (*rejectingAddQbit) Torrent(context.Context, string) (qbittorrent.Torrent, error) {
	return qbittorrent.Torrent{}, errors.New("qBittorrent torrent was not found")
}

func (*rejectingAddQbit) AddTorrent(context.Context, []byte, qbittorrent.AddTorrentOptions) error {
	return &qbittorrent.AddTorrentRejectedError{FailureCount: 1}
}

func TestDefiniteAddRejectionReturnsToExplicitRetryPoint(t *testing.T) {
	t.Parallel()
	state, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("torrent artifact")
	digest := sha256.Sum256(raw)
	artifact := hex.EncodeToString(digest[:])
	if err := state.saveArtifact(artifact, raw); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{
		qbittorrent: &rejectingAddQbit{}, store: state,
		remoteMediaRoot: "/downloads", sonarrMediaRoot: "/downloads",
	}
	release := Release{
		ID: "show-s01", Status: "updating",
		CurrentRevision:   Revision{TorrentID: "1111111111111111111111111111111111111111", SavePath: "/downloads/current", SonarrSavePath: "/downloads/current"},
		CandidateRevision: &Revision{TorrentID: "2222222222222222222222222222222222222222", ArtifactSHA256: artifact, SavePath: "/downloads/candidate", SonarrSavePath: "/downloads/candidate"},
		Operation:         &Operation{Phase: "copied", UpdatedAt: time.Now()},
	}
	progressed, waiting, err := engine.advanceOne(context.Background(), &release)
	if err == nil || !progressed || waiting || release.Status != "blocked" || release.Operation.Phase != "copied" {
		t.Fatalf("definite rejection = progressed %t, waiting %t, status %q, phase %q, error %v", progressed, waiting, release.Status, release.Operation.Phase, err)
	}
}

var _ qbittorrentAPI = (*candidateQbit)(nil)
