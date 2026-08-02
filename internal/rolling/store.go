package rolling

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

var errLocked = errors.New("another import or rolling operation holds the persistent execution lock")

type store struct {
	root      string
	releases  string
	artifacts string
	lockPath  string
}

type lock struct{ file *os.File }

func newStore(dataRoot string) (*store, error) {
	result := &store{
		root:     filepath.Join(dataRoot, "rolling"),
		lockPath: filepath.Join(dataRoot, "execute.lock"),
	}
	result.releases = filepath.Join(result.root, "releases")
	result.artifacts = filepath.Join(result.root, "artifacts")
	for _, directory := range []string{result.root, result.releases, result.artifacts} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create rolling state directory: %w", err)
		}
	}
	for _, directory := range []string{dataRoot, result.root, result.releases, result.artifacts} {
		handle, err := os.Open(directory)
		if err != nil {
			return nil, fmt.Errorf("open rolling state directory for sync: %w", err)
		}
		syncErr := handle.Sync()
		closeErr := handle.Close()
		if syncErr != nil {
			return nil, fmt.Errorf("sync rolling state directory: %w", syncErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close rolling state directory: %w", closeErr)
		}
	}
	return result, nil
}

func (s *store) tryLock() (*lock, error) {
	file, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open execution lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errLocked
		}
		return nil, fmt.Errorf("acquire execution lock: %w", err)
	}
	return &lock{file: file}, nil
}

func (l *lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func (s *store) save(release Release) error {
	release.Version = recordVersion
	release.UpdatedAt = time.Now().UTC()
	encoded, err := json.MarshalIndent(release, "", "  ")
	if err != nil {
		return fmt.Errorf("encode rolling release: %w", err)
	}
	encoded = append(encoded, '\n')
	return writeAtomic(s.releases, s.releasePath(release.ID), encoded)
}

func (s *store) load(id string) (Release, bool, error) {
	encoded, err := os.ReadFile(s.releasePath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Release{}, false, nil
	}
	if err != nil {
		return Release{}, false, fmt.Errorf("read rolling release: %w", err)
	}
	var release Release
	if err := json.Unmarshal(encoded, &release); err != nil {
		return Release{}, false, fmt.Errorf("decode rolling release: %w", err)
	}
	if release.Version != recordVersion || release.ID != id || release.CurrentRevision.TorrentID == "" {
		return Release{}, false, fmt.Errorf("rolling release %q has an unsupported or incomplete record", id)
	}
	return release, true, nil
}

func (s *store) list() ([]Release, error) {
	entries, err := os.ReadDir(s.releases)
	if err != nil {
		return nil, fmt.Errorf("list rolling releases: %w", err)
	}
	result := make([]Release, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		release, exists, err := s.load(id)
		if err != nil {
			return nil, err
		}
		if exists {
			result = append(result, release)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *store) saveArtifact(digest string, raw []byte) error {
	computed := sha256.Sum256(raw)
	if digest != hex.EncodeToString(computed[:]) {
		return fmt.Errorf("rolling artifact SHA-256 does not match its content")
	}
	target := filepath.Join(s.artifacts, digest+".torrent")
	if existing, err := os.ReadFile(target); err == nil {
		if string(existing) != string(raw) {
			return fmt.Errorf("artifact digest collision for %s", digest)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read rolling artifact: %w", err)
	}
	return writeAtomic(s.artifacts, target, raw)
}

func (s *store) loadArtifact(digest string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(s.artifacts, digest+".torrent"))
	if err != nil {
		return nil, fmt.Errorf("read rolling artifact: %w", err)
	}
	computed := sha256.Sum256(raw)
	if digest != hex.EncodeToString(computed[:]) {
		return nil, fmt.Errorf("rolling artifact %s failed SHA-256 verification", digest)
	}
	return raw, nil
}

func (s *store) releasePath(id string) string {
	return filepath.Join(s.releases, id+".json")
}

func writeAtomic(directory, target string, data []byte) error {
	temporary, err := os.CreateTemp(directory, ".rolling-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary rolling state: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
