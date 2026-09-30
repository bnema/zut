package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, dir, name, desc string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + desc + "\n---\nbody of " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
}

func TestDiscoverFollowsSymlinkedSkillDirectories(t *testing.T) {
	t.Setenv("ZUT_AGENT_SKILLS", "")
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	shared := filepath.Join(tmp, "shared")
	writeSkill(t, filepath.Join(shared, "linked"), "linked", "from checkout")
	writeSkill(t, filepath.Join(shared, "group", "inner"), "inner", "nested")
	root := filepath.Join(home, ".agents", "skills")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, filepath.Join(shared, "linked"), filepath.Join(root, "linked"))
	symlinkOrSkip(t, filepath.Join(shared, "group"), filepath.Join(root, "bundle"))
	symlinkOrSkip(t, filepath.Join(tmp, "missing"), filepath.Join(root, "dangling"))

	list, errs := Discover("", filepath.Join(tmp, "proj"), home, true)
	if len(errs) != 0 {
		t.Fatalf("errors = %v", errs)
	}
	linked := FindByName(list, "linked")
	if linked == nil {
		t.Fatalf("linked skill missing: %v", list)
	}
	// The reported path is the installed (link) location.
	if want := filepath.Join(root, "linked", "SKILL.md"); linked.Path != want {
		t.Fatalf("path = %q, want %q", linked.Path, want)
	}
	inner := FindByName(list, "inner")
	if inner == nil {
		t.Fatal("skill under linked parent missing")
	}
	// Fork slash aliases follow the walked path and keep the frontmatter name.
	if FindByName(list, "bundle/inner") != inner {
		t.Fatalf("alias bundle/inner not registered: %#v", inner.Aliases)
	}
}

func TestDiscoverSymlinkedUserRoot(t *testing.T) {
	t.Setenv("ZUT_AGENT_SKILLS", "")
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real-skills")
	writeSkill(t, filepath.Join(real, "one"), "one", "d")
	home := filepath.Join(tmp, "home")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, real, filepath.Join(home, ".claude", "skills"))
	list, errs := Discover("", filepath.Join(tmp, "proj"), home, true)
	if len(errs) != 0 || FindByName(list, "one") == nil {
		t.Fatalf("skills=%v errs=%v", list, errs)
	}
}

func TestDiscoverSymlinkCycleTerminates(t *testing.T) {
	t.Setenv("ZUT_AGENT_SKILLS", "")
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	root := filepath.Join(home, ".agents", "skills")
	writeSkill(t, filepath.Join(root, "a"), "a", "d")
	symlinkOrSkip(t, root, filepath.Join(root, "a", "loop"))
	symlinkOrSkip(t, filepath.Join(root, "self"), filepath.Join(root, "self")) // ELOOP
	list, errs := Discover("", filepath.Join(tmp, "proj"), home, true)
	if len(errs) != 0 {
		t.Fatalf("errors = %v", errs)
	}
	n := 0
	for _, s := range list {
		if !s.Builtin {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("user skills = %d, want 1: %v", n, list)
	}
}

func TestDiscoverPrefersRealDirectoryOverLink(t *testing.T) {
	t.Setenv("ZUT_AGENT_SKILLS", "")
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	root := filepath.Join(home, ".agents", "skills")
	// "aaa" sorts before "real" but is a link to it; the real path must win.
	writeSkill(t, filepath.Join(root, "real"), "real", "d")
	symlinkOrSkip(t, filepath.Join(root, "real"), filepath.Join(root, "aaa"))
	list, errs := Discover("", filepath.Join(tmp, "proj"), home, true)
	if len(errs) != 0 {
		t.Fatalf("errors = %v", errs)
	}
	s := FindByName(list, "real")
	if s == nil || s.Path != filepath.Join(root, "real", "SKILL.md") {
		t.Fatalf("skill = %#v", s)
	}
}

func TestBundledSymlinkedEntriesStayConfinedToRoot(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "ext")
	bundle := filepath.Join(root, "skills")
	writeSkill(t, filepath.Join(root, "shared", "inside"), "inside", "in root")
	writeSkill(t, filepath.Join(tmp, "outside", "evil"), "evil", "outside root")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, filepath.Join(root, "shared", "inside"), filepath.Join(bundle, "inside"))
	symlinkOrSkip(t, filepath.Join(tmp, "outside", "evil"), filepath.Join(bundle, "evil"))
	symlinkOrSkip(t, filepath.Join(tmp, "nowhere"), filepath.Join(bundle, "dangling"))

	got, errs := LoadBundled([]BundledSkillDir{{Dir: bundle, Root: root, Source: "extension t"}})
	if FindByName(got, "inside") == nil {
		t.Fatalf("in-root link not followed: %v (errs %v)", got, errs)
	}
	if FindByName(got, "evil") != nil {
		t.Fatal("link escaped extension root")
	}
	if len(errs) == 0 {
		t.Fatal("expected escape diagnostic")
	}
}

func TestBundledSymlinkCycleAndDanglingAreSilentButRealDirsWin(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "ext")
	bundle := filepath.Join(root, "skills")
	// "aaa" sorts before "zzz" and links to it. Both carry the same
	// frontmatter name, so whichever loads first owns it: the real directory.
	realDir := filepath.Join(bundle, "zzz")
	writeSkill(t, realDir, "shared", "real")
	symlinkOrSkip(t, realDir, filepath.Join(bundle, "aaa"))
	symlinkOrSkip(t, filepath.Join(bundle, "loop"), filepath.Join(bundle, "loop"))
	symlinkOrSkip(t, filepath.Join(tmp, "nowhere"), filepath.Join(bundle, "dangling"))
	got, errs := LoadBundled([]BundledSkillDir{{Dir: bundle, Root: root, Source: "extension t"}})
	if len(errs) != 0 {
		t.Fatalf("dangling/cyclic links must be skipped silently: %v", errs)
	}
	s := FindByName(got, "shared")
	if s == nil || s.Path != filepath.Join(realDir, "SKILL.md") {
		t.Fatalf("real directory did not win: %#v", s)
	}
	if len(got) != 1 {
		t.Fatalf("skills = %v", got)
	}
}

func TestBundledSymlinkPermissionErrorIsReported(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced here")
	}
	tmp := t.TempDir()
	root := filepath.Join(tmp, "ext")
	bundle := filepath.Join(root, "skills")
	locked := filepath.Join(root, "locked")
	writeSkill(t, filepath.Join(locked, "hidden"), "hidden", "d")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, filepath.Join(locked, "hidden"), filepath.Join(bundle, "hidden"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	got, errs := LoadBundled([]BundledSkillDir{{Dir: bundle, Root: root, Source: "extension t"}})
	if len(got) != 0 || len(errs) == 0 {
		t.Fatalf("permission failure was swallowed: skills=%v errs=%v", got, errs)
	}
}

func TestDiscoverReportsUnreadableLinkTarget(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced here")
	}
	t.Setenv("ZUT_AGENT_SKILLS", "")
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	root := filepath.Join(home, ".agents", "skills")
	locked := filepath.Join(tmp, "locked")
	writeSkill(t, filepath.Join(locked, "x"), "x", "d")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, filepath.Join(locked, "x"), filepath.Join(root, "x"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	_, errs := Discover("", filepath.Join(tmp, "proj"), home, true)
	if len(errs) == 0 {
		t.Fatal("permission failure on a linked directory was swallowed")
	}
}

func userSkillCount(list []*Skill) int {
	n := 0
	for _, s := range list {
		if !s.Builtin {
			n++
		}
	}
	return n
}

func TestDiscoverIgnoresLinkToAncestorOfRoot(t *testing.T) {
	t.Setenv("ZUT_AGENT_SKILLS", "")
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	root := filepath.Join(home, ".agents", "skills")
	writeSkill(t, filepath.Join(root, "mine"), "mine", "d")
	// A sibling tree that must never be pulled in through an ancestor link.
	writeSkill(t, filepath.Join(home, "unrelated", "stray"), "stray", "d")
	symlinkOrSkip(t, home, filepath.Join(root, "up-home"))
	symlinkOrSkip(t, filepath.Join(home, ".agents"), filepath.Join(root, "up-agents"))
	symlinkOrSkip(t, root, filepath.Join(root, "self"))
	symlinkOrSkip(t, string(filepath.Separator), filepath.Join(root, "up-fs-root"))
	list, errs := Discover("", filepath.Join(tmp, "proj"), home, true)
	if len(errs) != 0 {
		t.Fatalf("errors = %v", errs)
	}
	if FindByName(list, "mine") == nil || FindByName(list, "stray") != nil || userSkillCount(list) != 1 {
		t.Fatalf("skills = %v", list)
	}
}

func TestDiscoverKeepsExternalSharedLinkThatDoesNotContainRoot(t *testing.T) {
	t.Setenv("ZUT_AGENT_SKILLS", "")
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	root := filepath.Join(home, ".agents", "skills")
	shared := filepath.Join(tmp, "dev", "skills-checkout")
	writeSkill(t, filepath.Join(shared, "team", "review"), "review", "d")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, shared, filepath.Join(root, "shared"))
	list, errs := Discover("", filepath.Join(tmp, "proj"), home, true)
	if len(errs) != 0 || FindByName(list, "review") == nil {
		t.Fatalf("skills=%v errs=%v", list, errs)
	}
}

func TestScanUserSkillDirBoundsDepthWithDiagnostic(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "skills")
	writeSkill(t, filepath.Join(root, "top"), "top", "d")
	deep := root
	for i := 0; i < maxSkillScanDepth+5; i++ {
		deep = filepath.Join(deep, "d")
	}
	writeSkill(t, deep, "too-deep", "d")
	seen := map[string]*Skill{}
	errs := scanUserSkillDir(location{dir: root, label: "test"}, seen)
	if seen["top"] == nil || seen["too-deep"] != nil {
		t.Fatalf("seen = %v", seen)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "skill scan stopped early") {
		t.Fatalf("errs = %v", errs)
	}
}

func TestScanUserSkillDirBoundsDirectoryCount(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "skills")
	writeSkill(t, filepath.Join(root, "a-first"), "first", "d")
	for i := 0; i < maxSkillScanDirs+50; i++ {
		if err := os.MkdirAll(filepath.Join(root, "z-bulk", fmt.Sprintf("d%05d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]*Skill{}
	errs := scanUserSkillDir(location{dir: root, label: "test"}, seen)
	if seen["first"] == nil {
		t.Fatal("skills found before the cap were dropped")
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "more than") {
		t.Fatalf("errs = %v", errs)
	}
}

func TestBundledRealAndAliasUnnamedSkillLoadsOnce(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "ext")
	bundle := filepath.Join(root, "skills")
	bar := filepath.Join(bundle, "bar")
	if err := os.MkdirAll(bar, 0o755); err != nil {
		t.Fatal(err)
	}
	// No frontmatter name: the directory name is the fallback.
	if err := os.WriteFile(filepath.Join(bar, "SKILL.md"), []byte("---\ndescription: d\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, bar, filepath.Join(bundle, "foo")) // sorts after "bar" only by luck; also test the reverse
	symlinkOrSkip(t, bar, filepath.Join(bundle, "aaa"))
	got, errs := LoadBundled([]BundledSkillDir{{Dir: bundle, Root: root, Source: "extension t"}})
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if len(got) != 1 || got[0].Name != "bar" || FindByName(got, "foo") != nil || FindByName(got, "aaa") != nil {
		t.Fatalf("alias duplicated the skill: %v", got)
	}
}
