package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bnema/zut/packages/agent/skills"
)

func TestSkillPinsPersistIndependentScopes(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	a, b := t.TempDir(), t.TempDir()
	for _, pin := range []struct {
		cwd, name string
		global    bool
	}{
		{a, "review", false}, {a, "review", true}, {b, "other", false},
	} {
		if err := toggleSkillPin(pin.cwd, pin.name, pin.global); err != nil {
			t.Fatal(err)
		}
	}
	if err := toggleSkillPin(a, "review", false); err != nil {
		t.Fatal(err)
	}
	pins, err := loadSkillPins(a)
	if err != nil || len(pins.Project) != 0 || !slices.Equal(pins.Global, []string{"review"}) {
		t.Fatalf("pins = %+v, %v", pins, err)
	}
	pins, err = loadSkillPins(b)
	if err != nil || !slices.Equal(pins.Project, []string{"other"}) || !slices.Equal(pins.Global, []string{"review"}) {
		t.Fatalf("pins = %+v, %v", pins, err)
	}
	info, err := os.Stat(filepath.Join(ZutHome(), skillPinsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if entries, err := os.ReadDir(a); err != nil || len(entries) != 0 {
		t.Fatalf("project was modified: %v, %v", entries, err)
	}
	// No temp files are left next to the preferences.
	entries, err := os.ReadDir(ZutHome())
	if err != nil || len(entries) != 1 {
		t.Fatalf("home entries = %v, %v", entries, err)
	}
}

func TestSkillPinsSavedSortedAndDeduplicated(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	cwd := t.TempDir()
	// A hand-edited file with duplicates and unsorted names loads normalized.
	key, err := skillPinProject(cwd)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(skillPinsFile{Global: []string{"b", "a", "b", " "}, Projects: map[string][]string{key: {"z", "a", "a"}}})
	if err := os.WriteFile(filepath.Join(ZutHome(), skillPinsFileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	pins, err := loadSkillPins(cwd)
	if err != nil || !slices.Equal(pins.Global, []string{"a", "b"}) || !slices.Equal(pins.Project, []string{"a", "z"}) {
		t.Fatalf("pins = %+v, %v", pins, err)
	}
	if err := toggleSkillPin(cwd, "m", false); err != nil {
		t.Fatal(err)
	}
	pins, _ = loadSkillPins(cwd)
	if !slices.Equal(pins.Project, []string{"a", "m", "z"}) {
		t.Fatalf("project = %v", pins.Project)
	}
	selected, warnings := pins.Resolve([]*skills.Skill{{Name: "z"}, {Name: "a"}, {Name: "b"}})
	if len(warnings) != 1 || len(selected) != 3 || selected[0].Name != "a" || selected[1].Name != "b" || selected[2].Name != "z" {
		t.Fatalf("selected=%v warnings=%v", selected, warnings)
	}
}

func TestSkillPinsDirectoryAliases(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	root := t.TempDir()
	target, alias := filepath.Join(root, "target"), filepath.Join(root, "alias")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := toggleSkillPin(target, "review", false); err != nil {
		t.Fatal(err)
	}
	pins, err := loadSkillPins(alias)
	if err != nil || !slices.Equal(pins.Project, []string{"review"}) {
		t.Fatalf("alias pins = %+v, %v", pins, err)
	}
}

func TestSkillPinsRejectCorruptFileWithoutOverwriting(t *testing.T) {
	for _, original := range []string{
		`{"global":["review"],"projects":42}`, `not json`, ``, `null`, ` null `, `[]`, `["review"]`, `42`, `"review"`, `true`,
		`{"global":["review"]} trailing`,
		`{"global":["review"],"future":{"x":1}}`, `{"version":2,"global":[]}`, `{"projects":{},"extra":null}`,
	} {
		t.Setenv("ZUT_HOME", t.TempDir())
		path := filepath.Join(ZutHome(), skillPinsFileName)
		if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}
		pins, err := loadSkillPins(t.TempDir())
		if err == nil || len(pins.Global) != 0 {
			t.Fatalf("%q: pins = %+v, err = %v", original, pins, err)
		}
		if strings.Contains(err.Error(), "review") {
			t.Fatalf("diagnostic echoes file contents: %v", err)
		}
		if err := toggleSkillPin(t.TempDir(), "new", true); err == nil {
			t.Fatal("corrupt file overwritten")
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, []byte(original)) {
			t.Fatalf("file changed: %q, %v", got, err)
		}
	}
}

func TestSkillPinsConcurrentTogglesKeepFileValid(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	cwd := t.TempDir()
	var wg sync.WaitGroup
	names := []string{"a", "b", "c", "d", "e", "f"}
	for _, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := toggleSkillPin(cwd, n, true); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	pins, err := loadSkillPins(cwd)
	if err != nil || !slices.Equal(pins.Global, names) {
		t.Fatalf("pins = %+v, %v", pins, err)
	}
}

func TestPreloadSkillPinsFreshOnlyAndNoSkill(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("ZUT_AGENT_SKILLS", "")
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".zut", "skills", "review")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: review\ndescription: Review things.\ndisable-model-invocation: true\n---\nInspect carefully.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, pin := range []struct {
		name   string
		global bool
	}{{"review", true}, {"review", false}, {"missing", false}} {
		if err := toggleSkillPin(cwd, pin.name, pin.global); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name            string
		fresh, disabled bool
	}{
		{"fresh", true, false}, {"resumed", false, false}, {"disabled", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var diagnostics bytes.Buffer
			args := Args{CWD: cwd, WithSkills: true, NoSkill: tt.disabled}
			got := preloadSkillPins(args, tt.fresh, "question", nil, &diagnostics)
			if tt.fresh && !tt.disabled {
				if strings.Count(got, "# Skill: review") != 1 || !strings.Contains(got, "Inspect carefully.") || !strings.HasSuffix(got, "User request:\nquestion") {
					t.Fatal(got)
				}
				if !strings.Contains(diagnostics.String(), "missing") {
					t.Fatal("missing warning")
				}
			} else if got != "question" || diagnostics.Len() != 0 {
				t.Fatalf("prompt = %q, warnings = %q", got, diagnostics.String())
			}
		})
	}
}

func TestPreloadSkillPinsMalformedFileWarnsAndKeepsPrompt(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(ZutHome(), skillPinsFileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	got := preloadSkillPins(Args{CWD: t.TempDir(), WithSkills: true}, true, "q", nil, &diagnostics)
	if got != "q" || !strings.Contains(diagnostics.String(), "invalid") {
		t.Fatalf("got=%q diagnostics=%q", got, diagnostics.String())
	}
}

func TestPreloadSkillPinsUsesExtensionSkills(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("ZUT_AGENT_SKILLS", "")
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	if err := toggleSkillPin(cwd, "ext-skill", true); err != nil {
		t.Fatal(err)
	}
	bundled := []*skills.Skill{{Name: "ext-skill", Body: "from extension", Source: "extension x"}}
	got := preloadSkillPins(Args{CWD: cwd, WithSkills: true}, true, "q", bundled, &bytes.Buffer{})
	if !strings.Contains(got, "from extension") {
		t.Fatal(got)
	}
}

func TestSkillPinsSymlinkedFileIsReadableButNeverReplaced(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	target := filepath.Join(t.TempDir(), "dotfiles-skill-pins.json")
	original := []byte("{\"global\":[\"review\"]}\n")
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ZutHome(), skillPinsFileName)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	pins, err := loadSkillPins(t.TempDir())
	if err != nil || !slices.Equal(pins.Global, []string{"review"}) {
		t.Fatalf("read through link: %+v, %v", pins, err)
	}
	err = toggleSkillPin(t.TempDir(), "other", true)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v", err)
	}
	if info, lerr := os.Lstat(link); lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link was replaced: %v, %v", info, lerr)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, original) {
		t.Fatalf("target changed: %q", got)
	}
	entries, _ := os.ReadDir(ZutHome())
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

func TestSkillPinsUnknownFieldsFailClosedWithClearError(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	path := filepath.Join(ZutHome(), skillPinsFileName)
	original := []byte(`{"global":["a"],"notes":"keep me"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadSkillPins(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "global") || strings.Contains(err.Error(), "keep me") {
		t.Fatalf("err = %v", err)
	}
	if err := toggleSkillPin(t.TempDir(), "b", false); err == nil {
		t.Fatal("toggle accepted unknown fields")
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, original) {
		t.Fatalf("file changed: %q", got)
	}
	// The documented schema still round-trips.
	if err := os.WriteFile(path, []byte(`{"global":["a"],"projects":{"/x":["b"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := toggleSkillPin(t.TempDir(), "c", true); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"/x"`) {
		t.Fatalf("other project pins lost: %s", raw)
	}
}
