package rolling

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

func (e *Engine) safeLocalPath(remote string) (string, error) {
	remote = path.Clean(remote)
	relative := ""
	if remote != e.remoteMediaRoot {
		prefix := e.remoteMediaRoot + "/"
		if !strings.HasPrefix(remote, prefix) {
			return "", fmt.Errorf("remote path %q is outside QBITTORRENT_MEDIA_ROOT", remote)
		}
		relative = strings.TrimPrefix(remote, prefix)
	}
	if strings.HasPrefix(relative, "../") || strings.HasPrefix(relative, "/") {
		return "", fmt.Errorf("remote path %q is outside QBITTORRENT_MEDIA_ROOT", remote)
	}
	local := filepath.Clean(filepath.Join(e.localMediaRoot, filepath.FromSlash(relative)))
	localRelative, err := filepath.Rel(filepath.Clean(e.localMediaRoot), local)
	if err != nil || localRelative == ".." || strings.HasPrefix(localRelative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("translated local path escapes IMPORTER_MEDIA_ROOT")
	}
	return local, nil
}

func (e *Engine) digestRemoteFile(ctx context.Context, remote string, expectedSize int64) (string, error) {
	local, err := e.safeLocalPath(remote)
	if err != nil {
		return "", err
	}
	return digestRegularFile(ctx, e.localMediaRoot, local, expectedSize)
}

func digestRegularFile(ctx context.Context, root, target string, expectedSize int64) (string, error) {
	if err := rejectSymlinkChain(root, target); err != nil {
		return "", err
	}
	info, err := os.Lstat(target)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() != expectedSize {
		return "", fmt.Errorf("path is not a regular file of expected size %d", expectedSize)
	}
	file, err := os.Open(target)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", fmt.Errorf("file identity changed while opening")
	}
	hash := sha256.New()
	written, err := copyContext(ctx, hash, file)
	if err != nil {
		return "", err
	}
	if written != expectedSize {
		return "", fmt.Errorf("read %d bytes, expected %d", written, expectedSize)
	}
	closedOver, err := os.Lstat(target)
	if err != nil || !os.SameFile(opened, closedOver) {
		return "", fmt.Errorf("file identity changed while hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (e *Engine) ensureStagingSpace(revision Revision) error {
	local, err := e.safeLocalPath(revision.SavePath)
	if err != nil {
		return err
	}
	if err := ensureSafeDirectory(e.localMediaRoot, local); err != nil {
		return err
	}
	var stats syscall.Statfs_t
	if err := syscall.Statfs(local, &stats); err != nil {
		return fmt.Errorf("inspect staging filesystem capacity: %w", err)
	}
	available := int64(stats.Bavail) * int64(stats.Bsize)
	required := revision.TotalLength + revision.TotalLength/100
	if available < required {
		return fmt.Errorf("rolling staging requires %d bytes but only %d bytes are available", required, available)
	}
	return nil
}

func (e *Engine) copyOwnedFile(ctx context.Context, release *Release, candidateIndex int) error {
	candidate := &release.CandidateRevision.Files[candidateIndex]
	if candidate.EpisodeID == 0 || candidate.ImportNeeded {
		candidate.Copied = true
		return nil
	}
	var current *RevisionFile
	for index := range release.CurrentRevision.Files {
		if release.CurrentRevision.Files[index].EpisodeID == candidate.EpisodeID {
			current = &release.CurrentRevision.Files[index]
			break
		}
	}
	if current == nil || current.ContentSHA256 == "" {
		return fmt.Errorf("no current content receipt for episode %d", candidate.EpisodeID)
	}
	sourceRemote := path.Join(release.CurrentRevision.SavePath, current.CurrentPath)
	targetRemote := path.Join(release.CandidateRevision.SavePath, candidate.RawPath)
	source, err := e.safeLocalPath(sourceRemote)
	if err != nil {
		return err
	}
	target, err := e.safeLocalPath(targetRemote)
	if err != nil {
		return err
	}
	sourceDigest, err := digestRegularFile(ctx, e.localMediaRoot, source, candidate.Size)
	if err != nil {
		return fmt.Errorf("verify current reuse source: %w", err)
	}
	if sourceDigest != current.ContentSHA256 {
		return fmt.Errorf("current source content changed since rolling enrollment for episode %d", candidate.EpisodeID)
	}
	if info, statErr := os.Lstat(target); statErr == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("staging target %q is not a regular file", target)
		}
		targetDigest, digestErr := digestRegularFile(ctx, e.localMediaRoot, target, candidate.Size)
		if digestErr != nil || targetDigest != sourceDigest {
			return fmt.Errorf("existing staging target %q does not match the owned source", target)
		}
		candidate.Copied = true
		return nil
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	if err := copyRegularFileAtomic(ctx, e.localMediaRoot, source, target, candidate.Size); err != nil {
		return err
	}
	targetDigest, err := digestRegularFile(ctx, e.localMediaRoot, target, candidate.Size)
	if err != nil || targetDigest != sourceDigest {
		return fmt.Errorf("copied staging file did not verify")
	}
	candidate.Copied = true
	return nil
}

func copyRegularFileAtomic(ctx context.Context, root, source, target string, expectedSize int64) error {
	if err := rejectSymlinkChain(root, source); err != nil {
		return err
	}
	if err := ensureSafeDirectory(root, filepath.Dir(target)); err != nil {
		return err
	}
	if err := cleanupRollingCopyTemps(filepath.Dir(target)); err != nil {
		return err
	}
	sourceInfo, err := os.Lstat(source)
	if err != nil || !sourceInfo.Mode().IsRegular() || sourceInfo.Size() != expectedSize {
		return fmt.Errorf("reuse source is not a regular file of expected size")
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	openedInfo, err := input.Stat()
	if err != nil || !os.SameFile(sourceInfo, openedInfo) {
		return fmt.Errorf("reuse source identity changed while opening")
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".rolling-copy-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	directoryInfo, err := os.Lstat(filepath.Dir(target))
	if err != nil {
		cleanup()
		return err
	}
	sharedGID, err := fileGroupID(directoryInfo)
	if err != nil {
		cleanup()
		return err
	}
	if err := temporary.Chown(-1, sharedGID); err != nil {
		cleanup()
		return fmt.Errorf("assign shared media group to staging copy: %w", err)
	}
	if err := temporary.Chmod(0o660); err != nil {
		cleanup()
		return err
	}
	written, err := copyContext(ctx, temporary, input)
	if err != nil || written != expectedSize {
		cleanup()
		if err != nil {
			return fmt.Errorf("copy reuse source: wrote %d of %d bytes: %w", written, expectedSize, err)
		}
		return fmt.Errorf("copy reuse source: wrote %d of %d bytes", written, expectedSize)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("staging target appeared during copy")
	} else if !os.IsNotExist(err) {
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	directory, err := os.Open(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 1024*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			count, writeErr := destination.Write(buffer[:read])
			written += int64(count)
			if writeErr != nil {
				return written, writeErr
			}
			if count != read {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return written, nil
			}
			return written, readErr
		}
	}
}

func cleanupRollingCopyTemps(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("inspect staging directory for interrupted copies: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".rolling-copy-") || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		temporaryPath := filepath.Join(directory, name)
		info, err := os.Lstat(temporaryPath)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("interrupted rolling copy path %q is not a regular file", temporaryPath)
		}
		if err := os.Remove(temporaryPath); err != nil {
			return fmt.Errorf("remove interrupted rolling copy %q: %w", temporaryPath, err)
		}
	}
	return nil
}

func ensureSafeDirectory(root, target string) error {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("directory escapes importer media root")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("IMPORTER_MEDIA_ROOT is not a real directory")
	}
	sharedGID, err := fileGroupID(rootInfo)
	if err != nil {
		return err
	}
	current := root
	if relative == "." {
		return nil
	}
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err := os.Mkdir(current, 0o770); err != nil && !os.IsExist(err) {
				return err
			}
			if err := os.Chmod(current, 0o770|os.ModeSetgid); err != nil {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("staging directory %q is not a real directory", current)
		}
		currentGID, err := fileGroupID(info)
		if err != nil {
			return err
		}
		needsChmod := info.Mode().Perm()&0o070 != 0o070 || info.Mode()&os.ModeSetgid == 0
		if currentGID != sharedGID {
			if err := os.Chown(current, -1, sharedGID); err != nil {
				return fmt.Errorf("assign shared media group to staging directory %q: %w", current, err)
			}
			needsChmod = true
		}
		desiredMode := info.Mode().Perm() | 0o070 | os.ModeSetgid
		if needsChmod {
			if err := os.Chmod(current, desiredMode); err != nil {
				return fmt.Errorf("make staging directory %q group-writable: %w", current, err)
			}
		}
	}
	return nil
}

func fileGroupID(info os.FileInfo) (int, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("filesystem does not expose a Unix group ID")
	}
	return int(stat.Gid), nil
}

func rejectSymlinkChain(root, target string) error {
	root = filepath.Clean(root)
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("IMPORTER_MEDIA_ROOT is not a real directory")
	}
	relative, err := filepath.Rel(root, filepath.Clean(target))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes importer media root")
	}
	current := root
	components := []string{}
	if relative != "." {
		components = strings.Split(relative, string(filepath.Separator))
	}
	for _, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component %q is a symlink", current)
		}
	}
	return nil
}
