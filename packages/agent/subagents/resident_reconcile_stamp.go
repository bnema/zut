package subagents

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Bump this whenever reconciliation validation, translation, or repair logic
// changes: a stamp certifies that this version has checked the entire journal.
const residentReconcileFormatVersion = 1
const residentReconcileStampName = "reconciled.json"

type residentProjectionFingerprint struct {
	Present bool     `json:"present"`
	Hash    [32]byte `json:"hash"`
}

type residentReconcileStamp struct {
	Version  int                           `json:"version"`
	Size     int64                         `json:"size"`
	ModTime  time.Time                     `json:"mtime"`
	Spec     ResidentChildSpec             `json:"spec"`
	Metadata residentProjectionFingerprint `json:"metadata"`
	Result   residentProjectionFingerprint `json:"result"`
}

func residentSettled(state ResidentState) bool {
	switch state {
	case ResidentIdle, ResidentCompleted, ResidentFailed, ResidentInterrupted:
		return true
	default:
		return false
	}
}

func residentProjectionFingerprintAt(dir, name string) (residentProjectionFingerprint, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return residentProjectionFingerprint{}, nil
	}
	if err != nil {
		return residentProjectionFingerprint{}, err
	}
	return residentProjectionFingerprint{Present: true, Hash: sha256.Sum256(data)}, nil
}

// Both stamp operations require the journal lease. Projections are fingerprinted
// as well so deleting/changing a result or metadata still triggers its rebuild.
func readResidentReconcileStamp(dir string) (ResidentMetadata, ResidentChildSpec, bool) {
	data, err := os.ReadFile(filepath.Join(dir, residentReconcileStampName))
	if err != nil {
		return ResidentMetadata{}, ResidentChildSpec{}, false
	}
	var stamp residentReconcileStamp
	if json.Unmarshal(data, &stamp) != nil || stamp.Version != residentReconcileFormatVersion {
		return ResidentMetadata{}, ResidentChildSpec{}, false
	}
	info, err := os.Stat(filepath.Join(dir, residentTranscriptName))
	if err != nil || info.Size() != stamp.Size || !info.ModTime().Equal(stamp.ModTime) {
		return ResidentMetadata{}, ResidentChildSpec{}, false
	}
	metadata, err := ReadResidentMetadata(filepath.Join(dir, residentMetadataName))
	if err != nil || metadata.Version != residentJournalVersion || !residentSettled(metadata.State) || metadata.ID != stamp.Spec.ID || metadata.SessionID != stamp.Spec.SessionID || stamp.Spec.ID == "" {
		return ResidentMetadata{}, ResidentChildSpec{}, false
	}
	for name, want := range map[string]residentProjectionFingerprint{residentMetadataName: stamp.Metadata, residentResultName: stamp.Result} {
		got, err := residentProjectionFingerprintAt(dir, name)
		if err != nil || got != want {
			return ResidentMetadata{}, ResidentChildSpec{}, false
		}
	}
	return metadata, stamp.Spec, true
}

func writeResidentReconcileStamp(dir string, metadata ResidentMetadata, spec ResidentChildSpec) {
	if !residentSettled(metadata.State) {
		return
	}
	if err := persistResidentReconcileStamp(dir, spec); err != nil {
		slog.Warn("resident reconciliation stamp write failed", "child", metadata.ID, "error", err)
	}
}

func persistResidentReconcileStamp(dir string, spec ResidentChildSpec) error {
	info, err := os.Stat(filepath.Join(dir, residentTranscriptName))
	if err != nil {
		return err
	}
	metadata, err := residentProjectionFingerprintAt(dir, residentMetadataName)
	if err != nil {
		return err
	}
	result, err := residentProjectionFingerprintAt(dir, residentResultName)
	if err != nil {
		return err
	}
	return writeResidentProjection(dir, residentReconcileStampName, residentReconcileStamp{
		Version: residentReconcileFormatVersion, Size: info.Size(), ModTime: info.ModTime(), Spec: spec, Metadata: metadata, Result: result,
	})
}
