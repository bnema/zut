package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SessionFile is a candidate session file found without reading it.
type SessionFile struct {
	Path string
	Info os.FileInfo
}

// SessionFiles lists regular *.jsonl files in every cwd bucket of root's
// session namespace without reading them. Callers that cache eligibility
// (the search index) pair it with SessionListable for new or changed files.
// A missing sessions directory yields no files. Any other directory read
// failure is returned, so callers never mistake an unreadable bucket for
// deleted sessions.
func SessionFiles(ctx context.Context, root string) ([]SessionFile, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("session files: empty root")
	}
	sessionsDir := filepath.Join(root, "sessions")
	buckets, err := os.ReadDir(sessionsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session files: %w", err)
	}
	var files []SessionFile
	for _, bucket := range buckets {
		if err := contextErr(ctx); err != nil {
			return nil, err
		}
		if bucket.Type()&os.ModeSymlink != 0 || !bucket.IsDir() || !isSessionBucketName(bucket.Name()) {
			continue
		}
		dir := filepath.Join(sessionsDir, bucket.Name())
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue // bucket removed while listing
		}
		if err != nil {
			return nil, fmt.Errorf("session files: %w", err)
		}
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			info, err := entry.Info()
			if errors.Is(err, os.ErrNotExist) {
				continue // file removed while listing
			}
			if err != nil {
				return nil, fmt.Errorf("session files: %w", err)
			}
			if !info.Mode().IsRegular() {
				continue
			}
			files = append(files, SessionFile{Path: filepath.Join(dir, entry.Name()), Info: info})
		}
	}
	return files, nil
}

// SessionListable reports whether path is a session the Finder lists:
// valid metadata, not hidden, and stored in the bucket its cwd hashes to.
// It reads the whole file to find the latest metadata.
func SessionListable(ctx context.Context, root, path string) bool {
	meta, err := scanSessionMetaContext(ctx, path)
	if err != nil || meta.ID == "" || meta.CWD == "" || meta.HideFromSessions {
		return false
	}
	return filepath.Clean(SessionsDir(root, meta.CWD)) == filepath.Clean(filepath.Dir(path))
}

// ManagedSessionMeta validates that path is a regular session file directly
// owned by root's active namespace and returns its latest metadata. It is the
// trust boundary for a picker selection before a host changes workspace state.
func ManagedSessionMeta(ctx context.Context, root, path string) (SessionMeta, error) {
	if err := contextErr(ctx); err != nil {
		return SessionMeta{}, err
	}
	if strings.TrimSpace(root) == "" || strings.TrimSpace(path) == "" {
		return SessionMeta{}, errors.New("managed session: root or path is empty")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return SessionMeta{}, fmt.Errorf("managed session: stat: %w", err)
	}
	if !info.Mode().IsRegular() {
		return SessionMeta{}, errors.New("managed session: path is not a regular file")
	}
	meta, err := scanSessionMetaContext(ctx, path)
	if err != nil {
		return SessionMeta{}, err
	}
	if meta.ID == "" || meta.CWD == "" {
		return SessionMeta{}, errors.New("managed session: invalid session metadata")
	}
	wantDir := filepath.Clean(SessionsDir(root, meta.CWD))
	dirInfo, err := os.Lstat(wantDir)
	if err != nil || dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return SessionMeta{}, errors.New("managed session: cwd bucket is not a directory")
	}
	if filepath.Clean(filepath.Dir(path)) != wantDir {
		return SessionMeta{}, errors.New("managed session: cwd bucket does not match metadata")
	}
	return meta, nil
}
