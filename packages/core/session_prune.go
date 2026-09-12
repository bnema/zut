package core

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// StoredSessionGroup describes sessions whose metadata names the same working
// directory. Paths can span the default and named-agent session namespaces.
type StoredSessionGroup struct {
	CWD       string
	Paths     []string
	SizeBytes int64
}

// SessionScanIssue describes a stored entry that could not be safely
// classified. Callers should report these entries and leave them untouched.
type SessionScanIssue struct {
	Path string
	Err  error
}

// ScanStoredSessionGroups finds JSONL sessions below sessionsRoot and groups
// valid entries by the cwd recorded in their metadata. Symlinks and malformed
// sessions are reported as issues rather than followed or deleted.
func ScanStoredSessionGroups(sessionsRoot string) ([]StoredSessionGroup, []SessionScanIssue) {
	byCWD := make(map[string]*StoredSessionGroup)
	var issues []SessionScanIssue

	// A symlinked sessions root would otherwise scan as an empty store
	// with no hint; report it instead of silently finding nothing.
	if info, err := os.Lstat(sessionsRoot); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, []SessionScanIssue{{Path: sessionsRoot, Err: fmt.Errorf("sessions root is a symbolic link")}}
	}
	err := filepath.WalkDir(sessionsRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == sessionsRoot && errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			issues = append(issues, SessionScanIssue{Path: path, Err: walkErr})
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			issues = append(issues, SessionScanIssue{Path: path, Err: fmt.Errorf("symbolic link is not a stored session")})
			return nil
		}
		// Never block on or stream non-regular entries: a FIFO named
		// *.jsonl would hang open(2) forever and a device node would
		// return data without end. Report and preserve them instead.
		if !entry.Type().IsRegular() {
			issues = append(issues, SessionScanIssue{Path: path, Err: fmt.Errorf("not a regular file")})
			return nil
		}

		meta, err := readSessionMeta(path)
		if err != nil {
			issues = append(issues, SessionScanIssue{Path: path, Err: err})
			return nil
		}
		cwd := strings.TrimSpace(meta.CWD)
		if cwd == "" {
			issues = append(issues, SessionScanIssue{Path: path, Err: fmt.Errorf("session metadata has an empty cwd")})
			return nil
		}
		// Tree-navigation branches stay visible through /session tree;
		// pruning them here would silently remove sessions the picker
		// deliberately hides from the flat list.
		if meta.HideFromSessions {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			issues = append(issues, SessionScanIssue{Path: path, Err: err})
			return nil
		}
		group := byCWD[cwd]
		if group == nil {
			group = &StoredSessionGroup{CWD: cwd}
			byCWD[cwd] = group
		}
		group.Paths = append(group.Paths, path)
		group.SizeBytes += info.Size()
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		issues = append(issues, SessionScanIssue{Path: sessionsRoot, Err: err})
	}

	groups := make([]StoredSessionGroup, 0, len(byCWD))
	for _, group := range byCWD {
		sort.Strings(group.Paths)
		groups = append(groups, *group)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].CWD < groups[j].CWD })
	sort.Slice(issues, func(i, j int) bool { return issues[i].Path < issues[j].Path })
	return groups, issues
}
