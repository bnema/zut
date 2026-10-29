package detach

import (
	"os"
	"os/exec"
	"testing"
)

func TestConfigureRunsDetachedChild(t *testing.T) {
	if os.Getenv("ZUT_DETACH_CHILD") == "1" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestConfigureRunsDetachedChild$")
	cmd.Env = append(os.Environ(), "ZUT_DETACH_CHILD=1")
	Configure(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr not set")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("detached child failed: %v\n%s", err, out)
	}
}
