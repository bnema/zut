package subagents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/internal/flock"
	"github.com/google/uuid"
)

var ErrPruneParentUnresolved = errors.New("parent session could not be verified")

// ResidentPruneCandidate is a read-only preview, not authorization to delete.
type ResidentPruneCandidate struct {
	ID, Profile       string
	State             ResidentState
	UpdatedAt         time.Time
	SizeBytes         int64
	parentID          string
	rootInfo, dirInfo os.FileInfo
}

// ResidentPruner checks age and parent activity both during preview and under
// the existing journal lease. ParentActivity returns zero for a missing parent.
type ResidentPruner struct {
	Root           string
	Cutoff         time.Time
	ParentActivity func(string) (time.Time, error)
	// RecheckParentActivity optionally verifies parent identity afresh before staging.
	RecheckParentActivity func(string) (time.Time, error)
}

func (p ResidentPruner) openRoot() (*os.Root, os.FileInfo, error) {
	info, err := os.Lstat(p.Root)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, errors.New("subagents root is not a real directory")
	}
	root, err := os.OpenRoot(p.Root)
	if err != nil {
		return nil, nil, err
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(info, actual) {
		root.Close()
		return nil, nil, errors.New("subagents root changed")
	}
	return root, info, nil
}

// Scan never creates files or takes a lease. Busy children can appear in the
// preview; they will be skipped during deletion.
func (p ResidentPruner) Scan() ([]ResidentPruneCandidate, []error, error) {
	root, info, err := p.openRoot()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, nil, err
	}
	var candidates []ResidentPruneCandidate
	var notices []error
	for _, entry := range entries {
		id := entry.Name()
		if strings.HasPrefix(id, ".prune-") {
			notices = append(notices, fmt.Errorf("preserving staging entry %q; inspect it manually", id))
			continue
		}
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.String() != id || !entry.IsDir() {
			continue
		}
		dir, err := root.Lstat(id)
		if err != nil || !dir.IsDir() {
			continue
		}
		candidate, err := p.inspect(root, id, dir)
		if err != nil {
			if !errors.Is(err, ErrPruneParentUnresolved) {
				notices = append(notices, fmt.Errorf("preserving %s: %w", id, err))
			}
			continue
		}
		if candidate.ID != "" {
			candidate.rootInfo = info
			candidates = append(candidates, candidate)
		}
	}
	return candidates, notices, nil
}

func pruneRegular(root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) {
		f.Close()
		return nil, errors.New("file changed during inspection")
	}
	return f, nil
}

func pruneJSON(root *os.Root, name string, dst any) error {
	f, err := pruneRegular(root, name)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	// Require exactly one JSON value, with no trailing data.
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("unexpected trailing JSON data")
	}
	return nil
}

func pruneVersion(v int) bool { return v == residentJournalVersion || v == residentJournalVersionV2 }

func (p ResidentPruner) inspect(root *os.Root, id string, expected os.FileInfo) (ResidentPruneCandidate, error) {
	var c ResidentPruneCandidate
	child, err := root.OpenRoot(id)
	if err != nil {
		return c, err
	}
	defer child.Close()
	info, err := child.Stat(".")
	if err != nil || !os.SameFile(expected, info) {
		return c, errors.New("child directory changed")
	}
	var m ResidentMetadata
	if err := pruneJSON(child, residentMetadataName, &m); err != nil {
		return c, err
	}
	if !pruneVersion(m.Version) || m.ID != id || m.UpdatedAt.IsZero() || !m.UpdatedAt.Before(p.Cutoff) || (!residentSettled(m.State) && m.State != ResidentStopped) {
		return c, nil
	}
	for _, name := range []string{residentQuarantineName, residentPatchName, "worktree"} {
		if _, err := child.Lstat(name); !errors.Is(err, fs.ErrNotExist) {
			return c, nil
		}
	}
	if p.ParentActivity != nil {
		at, err := p.ParentActivity(m.ParentSessionID)
		if err != nil {
			return c, err
		}
		if at.After(p.Cutoff) {
			return c, nil
		}
	}
	f, err := pruneRegular(child, residentTranscriptName)
	if err != nil {
		return c, err
	}
	records, err := readResidentRecords(f, true)
	f.Close()
	if err != nil {
		return c, err
	}
	validated, err := validateResidentRecords(records)
	if err != nil {
		return c, err
	}
	if len(validated.dangling) != 0 {
		return c, nil // Reconciliation would repair unfinished tool calls.
	}
	latest := m.UpdatedAt
	for _, record := range records {
		if record.Time.After(latest) {
			latest = record.Time
		}
	}
	if !latest.Before(p.Cutoff) {
		return c, nil
	}
	spec, last, state, ok := prunableRecords(records, m)
	if !ok {
		return c, nil
	}
	var result ResidentResult
	if err := pruneJSON(child, residentResultName, &result); err == nil {
		result, err = validateResidentResult(result)
		if err != nil || result.ID != id || result.TurnID != last || result.State != state || result.PatchRef != "" {
			return c, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return c, err
	}
	entries, err := fs.ReadDir(child.FS(), ".")
	if err != nil {
		return c, err
	}
	var bytes int64
	for _, entry := range entries {
		file, err := child.Lstat(entry.Name())
		if err != nil {
			return c, err
		}
		// Resident state is flat. Do not recurse into subdirectories or mounts.
		if !file.Mode().IsRegular() || !pruneAllowedFile(entry.Name()) {
			return c, nil
		}
		bytes += file.Size()
	}
	if lock, err := child.Lstat(residentLeaseName); err != nil || !lock.Mode().IsRegular() {
		return c, nil
	}
	return ResidentPruneCandidate{ID: id, Profile: spec.Profile, State: m.State, UpdatedAt: latest, SizeBytes: bytes, dirInfo: info, parentID: m.ParentSessionID}, nil
}

func prunableRecords(records []residentRecord, m ResidentMetadata) (ResidentChildSpec, string, ResidentState, bool) {
	var empty ResidentChildSpec
	if len(records) == 0 || records[0].Type != residentRecordAccepted || records[0].Spec == nil {
		return empty, "", "", false
	}
	spec := *records[0].Spec
	if spec.ID != m.ID || spec.SessionID == "" || spec.SessionID != m.SessionID || spec.ParentSessionID != m.ParentSessionID || (spec.WorkspaceMode != "" && spec.WorkspaceMode != WorkspaceShared) || (spec.Required && m.State != ResidentIdle && m.State != ResidentCompleted) {
		return empty, "", "", false
	}
	turns := map[string]string{}
	if spec.InitialTurnID != "" {
		turns[spec.InitialTurnID] = residentRecordTurnAccepted
	}
	state := ResidentQueued
	last := ""
	for i, r := range records {
		if !pruneVersion(r.Version) || r.PatchRef != "" || r.Time.IsZero() {
			return empty, "", "", false
		}
		switch r.Type {
		case residentRecordAccepted, residentRecordTurnAccepted, residentRecordTurnStarted, residentRecordTurnFinished, residentRecordInterrupted, residentRecordFailed:
			if r.Time.After(m.UpdatedAt) {
				return empty, "", "", false
			}
		}
		previous := turns[r.TurnID]
		switch r.Type {
		case residentRecordAccepted:
			if i != 0 {
				return empty, "", "", false
			}
		case residentRecordTurnAccepted:
			if r.TurnID == "" || previous != "" {
				return empty, "", "", false
			}
			turns[r.TurnID] = r.Type
			state = ResidentQueued
		case residentRecordTurnStarted:
			if previous != residentRecordTurnAccepted {
				return empty, "", "", false
			}
			turns[r.TurnID] = r.Type
			state = ResidentRunning
		case residentRecordTurnFinished:
			if previous != residentRecordTurnStarted {
				return empty, "", "", false
			}
			turns[r.TurnID] = r.Type
			last = r.TurnID
			switch r.Outcome {
			case residentOutcomeCompleted:
				state = ResidentIdle
			case residentOutcomeFailed:
				state = ResidentFailed
			case residentV2OutcomeBudgetExhausted:
				if records[0].Version != residentJournalVersionV2 {
					return empty, "", "", false
				}
				state = ResidentFailed
			case residentV2OutcomeCompletedBudgetExhausted:
				if records[0].Version != residentJournalVersionV2 {
					return empty, "", "", false
				}
				state = ResidentCompleted
			default:
				return empty, "", "", false
			}
		case residentRecordInterrupted:
			if r.TurnID != "" {
				if previous != residentRecordTurnAccepted && previous != residentRecordTurnStarted {
					return empty, "", "", false
				}
				turns[r.TurnID] = r.Type
			}
			last = r.TurnID
			state = ResidentInterrupted
		case residentRecordFailed:
			if state != ResidentQueued {
				return empty, "", "", false
			}
			turns[spec.InitialTurnID] = r.Type
			last = ""
			state = ResidentFailed
		case residentRecordUser, residentRecordAssistant, residentRecordToolCall, residentRecordToolResult, residentRecordCompacted, residentRecordUsage, residentRecordGuard:
		default:
			return empty, "", "", false
		}
	}
	for _, turn := range turns {
		if turn == residentRecordTurnAccepted || turn == residentRecordTurnStarted {
			return empty, "", "", false
		}
	}
	return spec, last, state, state == m.State || (m.State == ResidentStopped && residentSettled(state))
}

// Delete revalidates the preview under the existing owner.lock, then detaches
// the directory from discovery before releasing the lease. Failed staging
// operations leave the original or staged directory for manual inspection.
func (p ResidentPruner) Delete(ctx context.Context, c ResidentPruneCandidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parsed, err := uuid.Parse(c.ID)
	if err != nil || parsed.String() != c.ID || c.rootInfo == nil || c.dirInfo == nil {
		return errors.New("invalid prune candidate")
	}
	root, info, err := p.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	if !os.SameFile(c.rootInfo, info) {
		return errors.New("subagents root changed")
	}
	dir, err := root.Lstat(c.ID)
	if err != nil || !dir.IsDir() || !os.SameFile(c.dirInfo, dir) {
		return errors.New("child directory changed")
	}
	name := filepath.Join(c.ID, residentLeaseName)
	lockInfo, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !lockInfo.Mode().IsRegular() {
		return errors.New("owner.lock is not a regular file")
	}
	f, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	actual, err := f.Stat()
	if err != nil || !os.SameFile(lockInfo, actual) {
		f.Close()
		return errors.New("owner.lock changed")
	}
	lease, err := flock.TryLockFile(f)
	if errors.Is(err, flock.ErrBusy) {
		return fmt.Errorf("in use: %w", ErrResidentLeaseBusy)
	}
	if err != nil {
		return err
	}
	defer lease.Close()
	checked, err := p.inspect(root, c.ID, dir)
	if err != nil {
		return err
	}
	if checked.ID == "" || !checked.UpdatedAt.Equal(c.UpdatedAt) || checked.State != c.State {
		return errors.New("child changed or no longer eligible")
	}
	stage := ".prune-" + uuid.NewString()
	if err := root.Mkdir(stage, 0o700); err != nil {
		return err
	}
	defer root.Remove(stage) // only removes an empty staging directory
	current, err := root.Lstat(c.ID)
	if err != nil || !current.IsDir() || !os.SameFile(dir, current) {
		return errors.New("child directory changed before staging")
	}
	currentLock, err := root.Lstat(name)
	if err != nil || !os.SameFile(lockInfo, currentLock) {
		return errors.New("owner.lock changed before staging")
	}
	recheck := p.RecheckParentActivity
	if recheck == nil {
		recheck = p.ParentActivity
	}
	if recheck != nil {
		at, err := recheck(checked.parentID)
		if err != nil {
			return err
		}
		if at.After(p.Cutoff) {
			return errors.New("parent became recently active")
		}
	}
	staged := filepath.Join(stage, c.ID)
	if err := ctx.Err(); err != nil {
		return err
	}
	// After detaching, finish cleanup even if cancellation arrives.
	if err := root.Rename(c.ID, staged); err != nil {
		return fmt.Errorf("cannot stage while holding lease (preserved): %w", err)
	}
	moved, err := root.Lstat(staged)
	if err != nil || !moved.IsDir() || !os.SameFile(dir, moved) {
		return fmt.Errorf("staged identity could not be verified; preserving %s", staged)
	}
	if err := lease.Close(); err != nil {
		return fmt.Errorf("release lease; preserving %s: %w", staged, err)
	}
	if err := cleanupPrunedDir(root, staged, dir); err != nil {
		return fmt.Errorf("detached, cleanup incomplete at %s: %w", staged, err)
	}
	return nil
}

func pruneAllowedFile(name string) bool {
	switch name {
	case residentMetadataName, residentTranscriptName, residentResultName, residentLeaseName, residentReconcileStampName:
		return true
	default:
		// Backups are intentionally retained rather than treating a filename
		// prefix as authorization to delete arbitrary contents.
		return false
	}
}

func cleanupPrunedDir(root *os.Root, staged string, expected os.FileInfo) error {
	child, err := root.OpenRoot(staged)
	if err != nil {
		return err
	}
	defer child.Close()
	info, err := child.Stat(".")
	if err != nil || !os.SameFile(expected, info) {
		return errors.New("staged directory changed")
	}
	entries, err := fs.ReadDir(child.FS(), ".")
	if err != nil {
		return err
	}
	files := make(map[string]os.FileInfo, len(entries))
	for _, entry := range entries {
		info, err := child.Lstat(entry.Name())
		if err != nil {
			return err
		}
		if !pruneAllowedFile(entry.Name()) || !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected entry %q", entry.Name())
		}
		files[entry.Name()] = info
	}
	for _, entry := range entries {
		info, err := child.Lstat(entry.Name())
		if err != nil || !info.Mode().IsRegular() || !os.SameFile(files[entry.Name()], info) {
			return fmt.Errorf("staged file %q changed", entry.Name())
		}
		if err := child.Remove(entry.Name()); err != nil {
			return err
		}
	}
	if err := child.Close(); err != nil {
		return err
	}
	current, err := root.Lstat(staged)
	if err != nil || !os.SameFile(expected, current) {
		return errors.New("staged directory changed before removal")
	}
	return root.Remove(staged)
}
