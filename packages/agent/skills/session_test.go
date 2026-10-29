package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestZutfileSkillsNoSetenv(t *testing.T) {
	t.Setenv("ZUT_AGENT_SKILLS", "user-supplied-value")
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(skillDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: session-local\ndescription: Session skill\n---\nBody"), 0600); err != nil {
		t.Fatal(err)
	}
	list, errs := Discover("", "", "", true, dir)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if FindByName(list, "session-local") == nil {
		t.Fatal("session skill missing")
	}
	if got := os.Getenv("ZUT_AGENT_SKILLS"); got != "user-supplied-value" {
		t.Fatalf("environment mutated: %q", got)
	}
	other, _ := Discover("", "", "", true)
	if FindByName(other, "session-local") != nil {
		t.Fatal("skill leaked to another session")
	}
}
