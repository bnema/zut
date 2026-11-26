package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxSessionSearchRowBytes bounds one JSONL row read for search text.
const maxSessionSearchRowBytes = 1 << 20

// SessionFile is a candidate session file found without reading it.
type SessionFile struct {
	Path string
	Info os.FileInfo
}

// SessionFiles lists regular *.jsonl files in every cwd bucket of root's
// session namespace without reading them. Callers that cache eligibility
// (the search index) pair it with SessionListable for new or changed files.
func SessionFiles(ctx context.Context, root string) []SessionFile {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	return sessionFilesIn(ctx, sessionBucketDirs(ctx, root))
}

// SessionListable reports whether path is a session the Finder lists:
// valid metadata, not hidden, and stored in the bucket its cwd hashes to.
// It reads the whole file to find the latest metadata.
func SessionListable(ctx context.Context, root, path string) bool {
	_, ok := listableSessionMeta(ctx, root, path)
	return ok
}

func listableSessionMeta(ctx context.Context, root, path string) (SessionMeta, bool) {
	meta, err := scanSessionMetaContext(ctx, path)
	if err != nil || meta.ID == "" || meta.CWD == "" || meta.HideFromSessions {
		return SessionMeta{}, false
	}
	if filepath.Clean(SessionsDir(root, meta.CWD)) != filepath.Clean(filepath.Dir(path)) {
		return SessionMeta{}, false
	}
	return meta, true
}

func sessionBucketDirs(ctx context.Context, root string) []string {
	sessionsDir := filepath.Join(root, "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		if contextErr(ctx) != nil {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() || !isSessionBucketName(entry.Name()) {
			continue
		}
		dirs = append(dirs, filepath.Join(sessionsDir, entry.Name()))
	}
	return dirs
}

func sessionFilesIn(ctx context.Context, dirs []string) []SessionFile {
	var files []SessionFile
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if contextErr(ctx) != nil {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			files = append(files, SessionFile{Path: filepath.Join(dir, entry.Name()), Info: info})
		}
	}
	return files
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
