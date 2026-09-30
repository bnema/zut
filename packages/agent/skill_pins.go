package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/bnema/zut/packages/agent/skills"
)

// skillPinsFileName lives directly under $ZUT_HOME. Pins are local
// preferences, so even project pins never write into a checkout.
const skillPinsFileName = "skill-pins.json"

type skillPinsFile struct {
	Global   []string            `json:"global,omitempty"`
	Projects map[string][]string `json:"projects,omitempty"`
}

// skillPinsMu serializes read-modify-write cycles within this process.
// Concurrent zut processes are not merged; the last atomic rename wins.
var skillPinsMu sync.Mutex

// userSkillSnapshot rediscovers the user-facing skills with the same
// precedence as agent construction (user/project locations, then
// extension-bundled skills). Built-ins are not pinnable and are filtered out.
// bundled is the running extension manager's skill list, or nil.
func userSkillSnapshot(args Args, cwd string, bundled []*skills.Skill) []*skills.Skill {
	if args.NoSkill {
		return nil
	}
	userHome, _ := os.UserHomeDir()
	list, _ := skills.Discover(ZutHome(), cwd, userHome, args.WithSkills)
	list = mergeExtensionSkills(skills.NewTool(list), bundled)
	return skills.VisibleSkills(list)
}

// preloadSkillPins folds the pinned skills' full bodies into the first prompt
// of a fresh non-interactive session. Resumed sessions and --no-skill runs
// return the prompt unchanged. Problems are reported as warnings on
// diagnostics (stderr), never on protocol output.
func preloadSkillPins(args Args, fresh bool, prompt string, bundled []*skills.Skill, diagnostics io.Writer) string {
	if !fresh || args.NoSkill {
		return prompt
	}
	pins, err := loadSkillPins(args.CWD)
	if err != nil {
		fmt.Fprintln(diagnostics, "warning:", err)
		return prompt
	}
	selected, warnings := pins.Resolve(userSkillSnapshot(args, args.CWD, bundled))
	for _, warning := range warnings {
		fmt.Fprintln(diagnostics, "warning:", warning)
	}
	return skills.PreloadPrompt(selected, prompt)
}

// skillPinProject returns the canonical project key: the absolute starting
// directory with symlinks resolved when possible (not the Git root).
func skillPinProject(cwd string) (string, error) {
	path, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path), nil
}

// normalizePinNames drops blanks and duplicates and sorts, so unions and
// saved files are deterministic.
func normalizePinNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func readSkillPins() (skillPinsFile, error) {
	var pins skillPinsFile
	data, err := os.ReadFile(filepath.Join(ZutHome(), skillPinsFileName))
	if errors.Is(err, os.ErrNotExist) {
		return pins, nil
	}
	if err != nil {
		return pins, fmt.Errorf("read skill pins: %w", err)
	}
	// Do not echo file contents in diagnostics. The root must be a JSON
	// object: json.Unmarshal accepts null (a silent no-op), which would let a
	// toggle replace a file that is not a preferences document. Unknown
	// top-level keys fail closed too, because saving would drop them.
	invalid := fmt.Errorf("invalid %s: expected an object with only \"global\" and \"projects\" (fix or remove it; it will not be overwritten)", skillPinsFileName)
	if !isJSONObject(data) {
		return skillPinsFile{}, invalid
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(&pins) != nil {
		return skillPinsFile{}, invalid
	}
	if _, err := dec.Token(); err != io.EOF { // trailing data
		return skillPinsFile{}, invalid
	}
	return pins, nil
}

func isJSONObject(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// loadSkillPins returns the global and current-project pins. On a malformed
// or unreadable file it returns the error and no pins.
func loadSkillPins(cwd string) (skills.Pins, error) {
	skillPinsMu.Lock()
	defer skillPinsMu.Unlock()
	project, err := skillPinProject(cwd)
	if err != nil {
		return skills.Pins{}, err
	}
	pins, err := readSkillPins()
	if err != nil {
		return skills.Pins{}, err
	}
	return skills.Pins{
		Global:  normalizePinNames(pins.Global),
		Project: normalizePinNames(pins.Projects[project]),
	}, nil
}

// toggleSkillPin flips one name in the global or current-project scope and
// saves via temp-file replacement. A malformed existing file is never
// overwritten.
func toggleSkillPin(cwd, name string, global bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("skill name is required")
	}
	skillPinsMu.Lock()
	defer skillPinsMu.Unlock()
	project, err := skillPinProject(cwd)
	if err != nil {
		return err
	}
	// A symlinked preferences file is read-only: renaming over it would
	// silently replace the link, and writing through it would modify a file
	// zut does not own. Loading may still follow it.
	path := filepath.Join(ZutHome(), skillPinsFileName)
	if info, lerr := os.Lstat(path); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link; skill pins can be read from it but not saved (replace it with a regular file to change pins)", skillPinsFileName)
	}
	pins, err := readSkillPins()
	if err != nil {
		return err
	}
	toggle := func(names []string) []string {
		names = normalizePinNames(names)
		if slices.Contains(names, name) {
			return slices.DeleteFunc(names, func(n string) bool { return n == name })
		}
		return normalizePinNames(append(names, name))
	}
	if global {
		pins.Global = toggle(pins.Global)
	} else {
		if pins.Projects == nil {
			pins.Projects = make(map[string][]string)
		}
		pins.Projects[project] = toggle(pins.Projects[project])
		if len(pins.Projects[project]) == 0 {
			delete(pins.Projects, project)
		}
	}
	data, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return err
	}
	return writeSkillPinsAtomic(path, append(data, '\n'))
}

// writeSkillPinsAtomic replaces the preferences file with data through a
// same-directory temp file (mode 0600 from CreateTemp), so readers never see
// a partial file.
func writeSkillPinsAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".skill-pins-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("save skill pins: %w", err)
	}
	return nil
}
