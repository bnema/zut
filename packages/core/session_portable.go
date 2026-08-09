package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bnema/zut/packages/provider"
	"github.com/google/uuid"
)

// PortableExt is the filesystem extension used for exported sessions.
// A ".zutsession" is just a zut JSONL session file with the meta
// header rewritten so the importing user gets fresh ownership.
const PortableExt = ".zutsession"

// ExportSession writes the session at srcPath to dstPath as a
// portable .zutsession file. If dstPath is an existing directory the
// file is created inside it with a name derived from the session's
// meta ("YYYYMMDD-HHMMSS-<first-prompt-excerpt>.zutsession"). The
// destination's directory is created if needed. Returns the final
// resolved path so the caller can tell the user where it landed.
//
// The on-disk format is unchanged from a live session; only the
// meta.cwd is stripped of its per-machine prefix (the importing
// user doesn't care what directory it came from). Everything else
// round-trips byte-for-byte.
func ExportSession(srcPath, dstPath string) (string, error) {
	if srcPath == "" {
		return "", errors.New("export: source path is empty")
	}
	if dstPath == "" {
		return "", errors.New("export: destination path is empty")
	}

	// Read the effective snapshot so a later model/provider update is not
	// replaced by the stale metadata in the first row.
	snapshot, err := ReadSessionSnapshot(srcPath)
	if err != nil {
		return "", fmt.Errorf("export: read session snapshot: %w", err)
	}

	// Read the source meta up-front so we can name the output sensibly
	// when dstPath is a directory, and so we can validate it's a real
	// session before starting to write.
	src, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("export: open source: %w", err)
	}
	defer src.Close()

	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64*1024), 20*1024*1024)
	if !sc.Scan() {
		return "", errors.New("export: session file is empty")
	}
	var head sessionLine
	if err := json.Unmarshal(sc.Bytes(), &head); err != nil {
		return "", fmt.Errorf("export: parse meta: %w", err)
	}
	if head.Type != "meta" || head.Meta == nil {
		return "", errors.New("export: first line is not a meta row")
	}

	// Scan the rest of the file for the first user message so we can
	// build a humane filename. Only reads if dstPath doesn't already
	// end in .zutsession.
	firstPrompt := ""
	if !strings.HasSuffix(strings.ToLower(dstPath), PortableExt) {
		if fi, _ := os.Stat(dstPath); fi == nil || fi.IsDir() {
			p, err := firstUserPrompt(src)
			if err != nil {
				return "", fmt.Errorf("export: read first prompt: %w", err)
			}
			firstPrompt = p
		}
	}

	// Resolve dstPath: if it's a directory, build a name inside it.
	outPath := dstPath
	if fi, err := os.Stat(dstPath); err == nil && fi.IsDir() {
		name := filenameFor(head.Meta.Started, head.Meta.ID, firstPrompt)
		outPath = filepath.Join(dstPath, name)
	} else if !strings.HasSuffix(strings.ToLower(outPath), PortableExt) {
		outPath += PortableExt
	}

	// Re-open the source from the top since we advanced the scanner.
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("export: rewind: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return "", fmt.Errorf("export: mkdir dst: %w", err)
	}
	dst, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("export: create dst: %w", err)
	}
	defer dst.Close()
	bw := bufio.NewWriter(dst)

	// Rewrite the meta row: strip the cwd (the importing user has
	// their own) and keep everything else identical. ID stays so the
	// export is traceable; the importer will rotate to a fresh ID.
	exportMeta := snapshot.Meta
	exportMeta.CWD = ""
	metaLine, err := json.Marshal(sessionLine{Type: "meta", Meta: &exportMeta})
	if err != nil {
		return "", fmt.Errorf("export: marshal meta: %w", err)
	}
	if _, err := bw.Write(metaLine); err != nil {
		return "", err
	}
	if err := bw.WriteByte('\n'); err != nil {
		return "", err
	}

	// Stream every non-meta row verbatim. Use ReadBytes instead of
	// bufio.Scanner: large sessions can contain very long JSONL rows
	// (image blocks, big tool outputs, compacted history) that exceed
	// Scanner's token limit and fail with "token too long".
	r := bufio.NewReader(src)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			var h sessionLineHead
			if err := json.Unmarshal(line, &h); err == nil && h.Type != "meta" {
				if _, werr := bw.Write(line); werr != nil {
					return "", werr
				}
				if werr := bw.WriteByte('\n'); werr != nil {
					return "", werr
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("export: read source: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		return "", err
	}
	return outPath, nil
}

// ImportSession copies the .zutsession file at srcPath into the
// running user's session store under the given root+cwd, rewriting
// the meta's id / cwd / started fields so the imported session is
// owned by the current user / directory / clock. Returns the path
// of the created session file, ready to pass to OpenSession.
//
// The imported session is a first-class zut session: it'll show up
// in /sessions, /jump, and on-disk summaries just like any other.
// Messages and usage rows are preserved verbatim.
func ImportSession(srcPath, root, cwd, version string) (string, error) {
	if srcPath == "" {
		return "", errors.New("import: source path is empty")
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("import: open source: %w", err)
	}
	defer src.Close()

	// Read the effective metadata before committing to a destination. Meta
	// rows are append-only, so the first row may describe an older provider or
	// model after a runtime switch.
	snapshot, err := ReadSessionSnapshot(srcPath)
	if err != nil {
		return "", fmt.Errorf("import: read session snapshot: %w", err)
	}

	// Validate the file header before committing to a destination.
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64*1024), 20*1024*1024)
	if !sc.Scan() {
		return "", errors.New("import: session file is empty")
	}
	var head sessionLine
	if err := json.Unmarshal(sc.Bytes(), &head); err != nil {
		return "", fmt.Errorf("import: parse meta: %w", err)
	}
	if head.Type != "meta" || head.Meta == nil {
		return "", errors.New("import: first line is not a meta row")
	}

	// Build the destination inside the current cwd's session dir
	// with a fresh timestamped name.
	dir := SessionsDir(root, cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	newID := uuid.NewString()
	name := fmt.Sprintf("%s-%s.jsonl", time.Now().UTC().Format("20060102-150405"), newID[:8])
	outPath := filepath.Join(dir, name)
	dst, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("import: create dst: %w", err)
	}
	defer dst.Close()
	bw := bufio.NewWriter(dst)

	// Write a fresh meta row claiming ownership.
	now := time.Now()
	timezone, timezoneOffset := localTimeMetadata(now)
	importMeta := SessionMeta{
		ID:             newID,
		CWD:            cwd,
		Model:          snapshot.Meta.Model,
		Provider:       snapshot.Meta.Provider,
		Started:        now.UTC(),
		Version:        version,
		Title:          snapshot.Title,
		Timezone:       timezone,
		TimezoneOffset: timezoneOffset,
		CompactHandoff: append(json.RawMessage(nil), snapshot.Meta.CompactHandoff...),
		Goal:           cloneSessionGoal(snapshot.Meta.Goal),
	}
	metaLine, err := json.Marshal(sessionLine{Type: "meta", Meta: &importMeta})
	if err != nil {
		return "", fmt.Errorf("import: marshal meta: %w", err)
	}
	if _, err := bw.Write(metaLine); err != nil {
		return "", err
	}
	if err := bw.WriteByte('\n'); err != nil {
		return "", err
	}

	// Rewind the source and stream every non-meta row. Avoid
	// bufio.Scanner so exported sessions with huge JSONL rows import
	// cleanly.
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("import: rewind: %w", err)
	}
	if err := forEachJSONLLine(src, func(line []byte) error {
		var h sessionLineHead
		if err := json.Unmarshal(line, &h); err != nil || h.Type == "meta" {
			return nil
		}
		if _, err := bw.Write(line); err != nil {
			return err
		}
		return bw.WriteByte('\n')
	}); err != nil {
		return "", fmt.Errorf("import: read source: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return "", err
	}
	return outPath, nil
}

// BranchSession creates a new session in root/cwd that contains the
// parent's messages 0..upToMessageIdx-1 (i.e. the first N user+
// assistant+tool rows). The new meta records Parent=<parent id> and
// ForkPoint=N so /session tree can rebuild the branch topology
// later. The effective repaired prefix is materialized, and all eligible
// usage checkpoints at or before the cut are preserved so the running cost
// tracker can reconstruct the final-turn delta. A boundary that ends on a
// tool call is extended through its paired tool result.
//
// upToMessageIdx is a count over the flat message stream as
// returned by OpenSession. To "branch at user turn 3" the caller
// passes the index of that user message in msgs + 1 (so the
// message itself is included). The caller figures that out; this
// helper rejects a count outside the current effective snapshot.
//
// Returns the path of the new session file, ready for OpenSession.
func BranchSession(parentPath, root, cwd, version string, upToMessageIdx int) (string, error) {
	return branchSession(parentPath, root, cwd, version, upToMessageIdx, false)
}

// BranchSessionHidden creates a branch that participates in /session tree but
// is hidden from the flat /sessions picker. Used for in-place tree navigation.
func BranchSessionHidden(parentPath, root, cwd, version string, upToMessageIdx int) (string, error) {
	return branchSession(parentPath, root, cwd, version, upToMessageIdx, true)
}

func branchSession(parentPath, root, cwd, version string, upToMessageIdx int, hideFromSessions bool) (string, error) {
	if parentPath == "" {
		return "", errors.New("branch: parent path is empty")
	}
	if upToMessageIdx < 0 {
		return "", errors.New("branch: upToMessageIdx must be >= 0")
	}

	snapshot, err := ReadSessionSnapshot(parentPath)
	if err != nil {
		return "", fmt.Errorf("branch: read parent snapshot: %w", err)
	}
	if upToMessageIdx > len(snapshot.Messages) {
		return "", fmt.Errorf("branch: upToMessageIdx %d exceeds effective message count %d", upToMessageIdx, len(snapshot.Messages))
	}

	// A tool-call row is not a safe provider boundary by itself. Accept the
	// convenient row index used by existing callers, but extend it through the
	// immediately paired tool result when necessary. Orphan calls already have
	// a synthetic result in the shared snapshot.
	limit := upToMessageIdx
	if limit > 0 && limit < len(snapshot.Messages) && messageHasToolCalls(snapshot.Messages[limit-1]) && snapshot.Messages[limit].Role == provider.RoleTool {
		limit++
	}

	extensionState, err := readExtensionStateAtFork(parentPath, limit)
	if err != nil {
		return "", err
	}
	compactHandoff := compactHandoffAtBranch(snapshot.Meta, snapshot.Messages, limit)
	goal := goalAtBranch(snapshot.Meta, snapshot.Messages, limit)
	return writeBranchSession(root, cwd, version, snapshot.Meta, snapshot.Messages, snapshot.UsageCheckpoints, limit, hideFromSessions, extensionState, compactHandoff, goal)
}

// BranchSessionHiddenFromHistory creates a hidden tree branch from a
// pre-compaction transcript segment. The segment is already repaired and is
// kept separate from the effective snapshot so normal resume semantics remain
// unchanged while older fork points stay usable.
func BranchSessionHiddenFromHistory(parentPath, root, cwd, version string, segment SessionHistorySegment, upToMessageIdx int) (string, error) {
	if parentPath == "" {
		return "", errors.New("branch: parent path is empty")
	}
	if upToMessageIdx < 0 {
		return "", errors.New("branch: upToMessageIdx must be >= 0")
	}
	if upToMessageIdx > len(segment.Messages) {
		return "", fmt.Errorf("branch: upToMessageIdx %d exceeds historical message count %d", upToMessageIdx, len(segment.Messages))
	}

	snapshot, err := ReadSessionSnapshot(parentPath)
	if err != nil {
		return "", fmt.Errorf("branch: read parent snapshot: %w", err)
	}
	limit := upToMessageIdx
	if limit > 0 && limit < len(segment.Messages) && messageHasToolCalls(segment.Messages[limit-1]) && segment.Messages[limit].Role == provider.RoleTool {
		limit++
	}
	extensionState, err := readExtensionStateAtFork(parentPath, limit)
	if err != nil {
		return "", err
	}
	return writeBranchSession(root, cwd, version, snapshot.Meta, segment.Messages, segment.UsageCheckpoints, limit, true, extensionState, nil, nil)
}

// readExtensionStateAtFork reconstructs the latest extension snapshot that was
// persisted at or before a branch boundary. Extension state is independent of
// provider messages, but its timeline still follows the effective message
// stream across compaction rows.
func readExtensionStateAtFork(path string, limit int) (map[string]json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("branch: open parent for extension state: %w", err)
	}
	defer f.Close()

	state := make(map[string]json.RawMessage)
	forkState := make(map[string]json.RawMessage)
	cloneState := func(source map[string]json.RawMessage) map[string]json.RawMessage {
		clone := make(map[string]json.RawMessage, len(source))
		for name, raw := range source {
			clone[name] = append(json.RawMessage(nil), raw...)
		}
		return clone
	}
	applyState := func(target map[string]json.RawMessage, row sessionLine) {
		if len(row.State) == 0 || strings.TrimSpace(string(row.State)) == "null" {
			delete(target, row.Extension)
			return
		}
		if json.Valid(row.State) {
			target[row.Extension] = append(json.RawMessage(nil), row.State...)
		}
	}
	effectiveCount := 0
	err = forEachStrictJSONLLine(f, func(line []byte, lineNo int) error {
		var head sessionLineHead
		if err := json.Unmarshal(line, &head); err != nil {
			return fmt.Errorf("line %d: invalid JSON: %w", lineNo, err)
		}
		switch head.Type {
		case "meta", "usage", "rename":
			return nil
		case "message":
			if _, err := hydrateMessage(line); err != nil {
				return fmt.Errorf("line %d: invalid message row: %w", lineNo, err)
			}
			effectiveCount++
		case "compaction":
			messages, err := hydrateCompaction(line)
			if err != nil {
				return fmt.Errorf("line %d: invalid compaction row: %w", lineNo, err)
			}
			effectiveCount = len(messages)
			// A compaction replaces the effective transcript. State that was
			// recorded anywhere before this boundary belongs to every fork
			// inside the replacement transcript, even when its old message
			// index was after the requested fork point.
			forkState = cloneState(state)
		case "extension_state":
			var row sessionLine
			if err := json.Unmarshal(line, &row); err != nil {
				return fmt.Errorf("line %d: invalid extension state row: %w", lineNo, err)
			}
			if row.Extension == "" || len(row.State) > maxExtensionStateBytes {
				return nil
			}
			applyState(state, row)
			if effectiveCount <= limit {
				applyState(forkState, row)
			}
		default:
			return fmt.Errorf("line %d: unknown row type %q", lineNo, head.Type)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("branch: read extension state: %w", err)
	}
	return forkState, nil
}

func compactHandoffAtBranch(parent SessionMeta, messages []provider.Message, limit int) json.RawMessage {
	if len(parent.CompactHandoff) == 0 || limit != len(messages) {
		return nil
	}
	return append(json.RawMessage(nil), parent.CompactHandoff...)
}

func goalAtBranch(parent SessionMeta, messages []provider.Message, limit int) *SessionGoal {
	if limit != len(messages) {
		return nil
	}
	return cloneSessionGoal(parent.Goal)
}

func cloneSessionGoal(goal *SessionGoal) *SessionGoal {
	if goal == nil {
		return nil
	}
	clone := *goal
	return &clone
}

func writeBranchSession(root, cwd, version string, parent SessionMeta, messages []provider.Message, checkpoints []SessionUsageCheckpoint, limit int, hideFromSessions bool, extensionState map[string]json.RawMessage, compactHandoff json.RawMessage, goal *SessionGoal) (string, error) {
	dir := SessionsDir(root, cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	newID := uuid.NewString()
	name := fmt.Sprintf("%s-%s.jsonl", time.Now().UTC().Format("20060102-150405"), newID[:8])
	outPath := filepath.Join(dir, name)
	// Build in a same-directory temporary file. Rename is atomic only when
	// the temporary and final paths share a directory; closing the file first
	// also ensures no buffered writer or descriptor can expose a partial branch.
	dst, err := os.CreateTemp(dir, ".zut-branch-*.tmp")
	if err != nil {
		return "", fmt.Errorf("branch: create temp: %w", err)
	}
	tmpPath := dst.Name()
	if err := dst.Chmod(0o644); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("branch: chmod temp: %w", err)
	}
	committed := false
	defer func() {
		_ = dst.Close()
		if !committed {
			_ = os.Remove(tmpPath)
			_ = os.Remove(outPath)
		}
	}()
	bw := bufio.NewWriter(dst)

	branchMeta := SessionMeta{
		ID:               newID,
		CWD:              cwd,
		Model:            parent.Model,
		Provider:         parent.Provider,
		Started:          time.Now().UTC(),
		Version:          version,
		Parent:           parent.ID,
		ForkPoint:        limit,
		HideFromSessions: hideFromSessions,
		CompactHandoff:   append(json.RawMessage(nil), compactHandoff...),
		Goal:             cloneSessionGoal(goal),
	}
	metaLine, err := json.Marshal(sessionLine{Type: "meta", Meta: &branchMeta})
	if err != nil {
		return "", fmt.Errorf("branch: marshal meta: %w", err)
	}
	if _, err := bw.Write(metaLine); err != nil {
		return "", err
	}
	if err := bw.WriteByte('\n'); err != nil {
		return "", err
	}

	for idx := 0; idx < limit; idx++ {
		msg := messages[idx]
		line, err := json.Marshal(sessionLine{Type: "message", Message: &msg})
		if err != nil {
			return "", fmt.Errorf("branch: marshal message %d: %w", idx, err)
		}
		if _, err := bw.Write(line); err != nil {
			return "", err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return "", err
		}
	}

	// Extension snapshots are branch state, not provider-visible transcript
	// content. Write the latest snapshot that existed at the fork point so a
	// child branch starts from the same extension state as its copied prefix.
	extensionNames := make([]string, 0, len(extensionState))
	for name := range extensionState {
		extensionNames = append(extensionNames, name)
	}
	sort.Strings(extensionNames)
	for _, name := range extensionNames {
		line, err := json.Marshal(sessionLine{Type: "extension_state", Extension: name, State: extensionState[name]})
		if err != nil {
			return "", fmt.Errorf("branch: marshal extension state: %w", err)
		}
		if _, err := bw.Write(line); err != nil {
			return "", fmt.Errorf("branch: write extension state: %w", err)
		}
		if err := bw.WriteByte('\n'); err != nil {
			return "", fmt.Errorf("branch: terminate extension state row: %w", err)
		}
	}
	// Preserve every cumulative checkpoint that belongs to this prefix. The
	// final two rows are needed to reconstruct LastTurnUsage as a delta.
	for _, checkpoint := range checkpoints {
		if checkpoint.MessageCount > limit {
			continue
		}
		usageLine, err := json.Marshal(sessionLine{
			Type:       "usage",
			Usage:      &checkpoint.Cumulative,
			Cumulative: &checkpoint.Cumulative,
		})
		if err != nil {
			return "", fmt.Errorf("branch: marshal usage: %w", err)
		}
		if _, err := bw.Write(usageLine); err != nil {
			return "", err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return "", err
		}
	}

	if err := bw.Flush(); err != nil {
		return "", err
	}
	if err := dst.Sync(); err != nil {
		return "", fmt.Errorf("branch: sync temp: %w", err)
	}
	if err := dst.Close(); err != nil {
		return "", fmt.Errorf("branch: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		return "", fmt.Errorf("branch: commit: %w", err)
	}
	committed = true
	return outPath, nil
}

func lastUserText(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != provider.RoleUser {
			continue
		}
		if text := strings.TrimSpace(firstTextFromMessage(msgs[i])); text != "" {
			return text
		}
	}
	return ""
}

// TreeNode is one entry in the branch tree returned by
// BuildSessionTree. Children are populated by linking on Parent ID.
type TreeNode struct {
	Summary  SessionSummary
	Meta     SessionMeta
	Children []*TreeNode
}

// BuildSessionTree loads every session in the cwd dir and returns
// the forest rooted at parentless sessions, with each non-root
// session placed under its parent. Used by /session tree to render
// the branch hierarchy.
func BuildSessionTree(root, cwd string) []*TreeNode {
	dir := SessionsDir(root, cwd)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	nodes := make(map[string]*TreeNode)
	order := []string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		summary := describeSession(path)
		meta, _ := readSessionMeta(path)
		if meta.ID == "" {
			continue
		}
		nodes[meta.ID] = &TreeNode{Summary: summary, Meta: meta}
		order = append(order, meta.ID)
	}
	return linkSessionTreeNodes(nodes, order)
}

// BuildSessionTreeStrict is the complete-read counterpart to the permissive
// forest builder. It validates every session file before linking any node, so
// callers that need a complete forest cannot silently omit malformed members.
func BuildSessionTreeStrict(root, cwd string) ([]*TreeNode, error) {
	dir := SessionsDir(root, cwd)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("build session tree: read directory: %w", err)
	}
	nodes := make(map[string]*TreeNode)
	order := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		snapshot, err := ReadSessionSnapshot(path)
		if err != nil {
			return nil, fmt.Errorf("build session tree: read %q: %w", path, err)
		}
		if snapshot.Meta.ID == "" {
			return nil, fmt.Errorf("build session tree: %q has no session id", path)
		}
		summary := SessionSummary{
			Path:          path,
			Started:       snapshot.Meta.Started,
			Model:         snapshot.Meta.Model,
			Provider:      snapshot.Meta.Provider,
			MessageCount:  len(snapshot.Messages),
			FirstUserText: firstUserTextFromMessages(snapshot.Messages),
			Title:         snapshot.Meta.Title,
		}
		if len(snapshot.UsageCheckpoints) > 0 {
			summary.TotalCost = snapshot.UsageCheckpoints[len(snapshot.UsageCheckpoints)-1].Cumulative.CostUSD
		}
		if _, exists := nodes[snapshot.Meta.ID]; exists {
			return nil, fmt.Errorf("build session tree: duplicate session id %q", snapshot.Meta.ID)
		}
		nodes[snapshot.Meta.ID] = &TreeNode{Summary: summary, Meta: snapshot.Meta}
		order = append(order, snapshot.Meta.ID)
	}
	return linkSessionTreeNodes(nodes, order), nil
}

// BuildSessionTreeFamilyStrict locates the current session using lightweight
// metadata headers, then strictly validates only that root and its descendants.
// A corrupt unrelated root in the same cwd bucket must not hide an otherwise
// readable current family, while a corrupt member of the selected family still
// fails the whole preflight.
func BuildSessionTreeFamilyStrict(root, cwd, currentPath string) ([]*TreeNode, error) {
	if currentPath == "" {
		return nil, errors.New("build session family: current path is empty")
	}
	dir := SessionsDir(root, cwd)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("build session family: read directory: %w", err)
	}
	cleanCurrent := filepath.Clean(currentPath)
	nodes := make(map[string]*TreeNode)
	order := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		meta, err := readSessionMetaHeader(path)
		if err != nil {
			if filepath.Clean(path) == cleanCurrent {
				return nil, fmt.Errorf("build session family: read current metadata %q: %w", path, err)
			}
			// A file whose header is unreadable cannot be associated with the
			// selected family. Valid descendants are linked below and fully
			// preflighted; unrelated corrupt roots stay out of this family.
			continue
		}
		if _, exists := nodes[meta.ID]; exists {
			return nil, fmt.Errorf("build session family: duplicate session id %q", meta.ID)
		}
		nodes[meta.ID] = &TreeNode{
			Summary: SessionSummary{Path: path, Started: meta.Started, Model: meta.Model, Provider: meta.Provider},
			Meta:    meta,
		}
		order = append(order, meta.ID)
	}
	roots := linkSessionTreeNodes(nodes, order)
	var familyRoot *TreeNode
	for _, candidate := range roots {
		if treeNodeContainsPath(candidate, cleanCurrent) {
			familyRoot = candidate
			break
		}
	}
	if familyRoot == nil {
		return nil, fmt.Errorf("build session family: current path %q is not in the session forest", currentPath)
	}
	var validate func(*TreeNode) error
	validate = func(node *TreeNode) error {
		snapshot, err := ReadSessionSnapshot(node.Summary.Path)
		if err != nil {
			return fmt.Errorf("build session family: read %q: %w", node.Summary.Path, err)
		}
		if snapshot.Meta.ID != node.Meta.ID {
			return fmt.Errorf("build session family: session id changed in %q", node.Summary.Path)
		}
		node.Meta = snapshot.Meta
		node.Summary = SessionSummary{
			Path:          node.Summary.Path,
			Started:       snapshot.Meta.Started,
			Model:         snapshot.Meta.Model,
			Provider:      snapshot.Meta.Provider,
			MessageCount:  len(snapshot.Messages),
			FirstUserText: firstUserTextFromMessages(snapshot.Messages),
			Title:         snapshot.Meta.Title,
		}
		if len(snapshot.UsageCheckpoints) > 0 {
			node.Summary.TotalCost = snapshot.UsageCheckpoints[len(snapshot.UsageCheckpoints)-1].Cumulative.CostUSD
		}
		for _, child := range node.Children {
			if err := validate(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := validate(familyRoot); err != nil {
		return nil, err
	}
	return []*TreeNode{familyRoot}, nil
}

func readSessionMetaHeader(path string) (SessionMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return SessionMeta{}, err
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return SessionMeta{}, err
	}
	line = bytes.TrimSpace(line)
	var row sessionLine
	if err := json.Unmarshal(line, &row); err != nil {
		return SessionMeta{}, err
	}
	if row.Type != "meta" || row.Meta == nil || row.Meta.ID == "" {
		return SessionMeta{}, errors.New("first row is not a valid meta row")
	}
	return *row.Meta, nil
}

func treeNodeContainsPath(node *TreeNode, path string) bool {
	if node == nil {
		return false
	}
	if filepath.Clean(node.Summary.Path) == filepath.Clean(path) {
		return true
	}
	for _, child := range node.Children {
		if treeNodeContainsPath(child, path) {
			return true
		}
	}
	return false
}

func linkSessionTreeNodes(nodes map[string]*TreeNode, order []string) []*TreeNode {
	var roots []*TreeNode
	for _, id := range order {
		n := nodes[id]
		if n.Meta.Parent == "" {
			roots = append(roots, n)
			continue
		}
		if parent, ok := nodes[n.Meta.Parent]; ok {
			parent.Children = append(parent.Children, n)
		} else {
			// Parent file missing (was manually deleted). Treat as a root so it
			// still shows up in the tree.
			roots = append(roots, n)
		}
	}
	return roots
}

// scanSessionMeta reads JSONL rows without hydrating transcript content and
// returns the latest valid metadata row. Listing and ID lookup only need this
// small projection; full snapshot validation remains the responsibility of
// resume, export, branching, and tree preflight callers.
func scanSessionMeta(path string) (meta SessionMeta, err error) {
	return scanSessionMetaContext(context.Background(), path)
}

func scanSessionMetaContext(ctx context.Context, path string) (meta SessionMeta, err error) {
	if err := contextErr(ctx); err != nil {
		return SessionMeta{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return SessionMeta{}, err
	}
	defer f.Close()

	var sawMeta bool
	err = forEachStrictJSONLLineContext(ctx, f, func(line []byte, lineNo int) error {
		var head sessionLineHead
		if err := json.Unmarshal(line, &head); err != nil {
			return fmt.Errorf("line %d: invalid JSON: %w", lineNo, err)
		}
		if head.Type == "" {
			return fmt.Errorf("line %d: missing row type", lineNo)
		}
		if !sawMeta && head.Type != "meta" {
			return fmt.Errorf("line %d: first row is not meta", lineNo)
		}
		switch head.Type {
		case "meta":
			var row struct {
				Meta *SessionMeta `json:"meta"`
			}
			if err := json.Unmarshal(line, &row); err != nil {
				return fmt.Errorf("line %d: invalid meta row: %w", lineNo, err)
			}
			if row.Meta == nil || row.Meta.ID == "" {
				return fmt.Errorf("line %d: meta row has no session id", lineNo)
			}
			meta = *row.Meta
			sawMeta = true
		case "message", "compaction", "usage", "rename", "extension_state":
			// These rows are intentionally not hydrated here.
		default:
			return fmt.Errorf("line %d: unknown row type %q", lineNo, head.Type)
		}
		return nil
	})
	if err != nil {
		return SessionMeta{}, fmt.Errorf("session metadata %q: %w", path, err)
	}
	if !sawMeta {
		return SessionMeta{}, fmt.Errorf("session metadata %q: file is empty", path)
	}
	return meta, nil
}

// readSessionMeta returns the latest validated metadata for path without
// reconstructing its transcript. Full snapshot validation is performed by
// callers that need messages rather than listing metadata.
func readSessionMeta(path string) (SessionMeta, error) {
	return scanSessionMeta(path)
}

// FindSessionByID looks up a session file in root/cwd whose meta id
// matches. Used by /session tree when the user picks an entry. O(n)
// over the files in the dir; the list is small in practice.
func FindSessionByID(root, cwd, id string) string {
	dir := SessionsDir(root, cwd)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		meta, err := scanSessionMeta(path)
		if err != nil {
			continue
		}
		if meta.ID == id {
			return path
		}
	}
	return ""
}

// FindManagedSessionByID looks up a session UUID across zut's managed
// session stores, independent of cwd. A nil error and empty path mean no
// matching session was found. Read, storage, metadata, and duplicate-ID
// failures are returned to the caller instead of being reported as a miss.
func FindManagedSessionByID(ctx context.Context, root, id string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("find managed session: root is empty")
	}
	if strings.TrimSpace(id) == "" {
		return "", errors.New("find managed session: id is empty")
	}

	stores := []string{root}
	agentsDir := filepath.Join(root, "sessions", "agents")
	agents, err := os.ReadDir(agentsDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("find managed session: read agent stores: %w", err)
	}
	if err == nil {
		for _, entry := range agents {
			if entry.IsDir() {
				stores = append(stores, filepath.Join(agentsDir, entry.Name()))
			}
		}
	}

	var matches []string
	for _, store := range stores {
		if err := contextErr(ctx); err != nil {
			return "", err
		}
		if err := collectManagedSessionMatches(ctx, store, id, &matches); err != nil {
			return "", err
		}
	}
	if err := contextErr(ctx); err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("find managed session: UUID %q is ambiguous", id)
	}
	return matches[0], nil
}

func collectManagedSessionMatches(ctx context.Context, root, id string, matches *[]string) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	dir := filepath.Join(root, "sessions")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find managed session: read %q: %w", dir, err)
	}
	for _, bucket := range entries {
		if err := contextErr(ctx); err != nil {
			return err
		}
		if !bucket.IsDir() || bucket.Name() == "agents" || !isSessionBucketName(bucket.Name()) {
			continue
		}
		bucketPath := filepath.Join(dir, bucket.Name())
		files, err := os.ReadDir(bucketPath)
		if err != nil {
			return fmt.Errorf("find managed session: read %q: %w", bucketPath, err)
		}
		for _, file := range files {
			if err := contextErr(ctx); err != nil {
				return err
			}
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(bucketPath, file.Name())
			meta, err := scanSessionMetaContext(ctx, path)
			if err != nil {
				return fmt.Errorf("find managed session: read metadata %q: %w", path, err)
			}
			if meta.ID == id {
				*matches = append(*matches, path)
			}
		}
	}
	return nil
}

func isSessionBucketName(name string) bool {
	if len(name) != 16 {
		return false
	}
	for _, r := range name {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// firstUserPrompt scans forward from the current source position
// looking for the first user-role message and returns its text.
// Used to build a humane export filename. Uses Reader instead of
// Scanner so a very large JSONL row before the first user prompt
// cannot trip Scanner's token limit.
func firstUserPrompt(src io.Reader) (string, error) {
	r := bufio.NewReader(src)
	for {
		lineBytes, err := r.ReadBytes('\n')
		if len(lineBytes) > 0 {
			lineBytes = bytes.TrimRight(lineBytes, "\r\n")
			var line sessionLine
			if err := json.Unmarshal(lineBytes, &line); err == nil {
				if line.Type == "message" && line.Message != nil && line.Message.Role == "user" {
					b, _ := json.Marshal(line.Message)
					var m struct {
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					}
					_ = json.Unmarshal(b, &m)
					for _, c := range m.Content {
						if c.Text != "" {
							return c.Text, nil
						}
					}
				}
			}
		}
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", err
		}
	}
}

// filenameFor builds a descriptive .zutsession filename from the
// session's start time and, when available, an excerpt of the
// first user prompt.
func filenameFor(started time.Time, id, firstPrompt string) string {
	base := started.UTC().Format("20060102-150405")
	if id != "" && len(id) >= 8 {
		base += "-" + id[:8]
	}
	slug := slugify(firstPrompt, 40)
	if slug != "" {
		base += "-" + slug
	}
	return base + PortableExt
}

// slugify lowercases, strips punctuation, collapses whitespace to
// hyphens, and truncates to max runes so it's safe as a filename.
func slugify(s string, max int) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return ""
	}
	var out strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && out.Len() > 0 {
				out.WriteByte('-')
				prevDash = true
			}
		}
		if out.Len() >= max {
			break
		}
	}
	return strings.TrimRight(out.String(), "-")
}
