package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtensionSpawnUsesSessionEnv(t *testing.T) {
	if os.Getenv("ZUT_L2_EXTENSION_HELPER") == "1" {
		cwd, _ := os.Getwd()
		data, _ := json.Marshal([]string{os.Getenv("ZUT_L2_VALUE"), cwd, os.Getenv("ZUT_L2_HOST_ONLY")})
		if err := os.WriteFile(os.Getenv("ZUT_L2_RESULT"), data, 0600); err != nil {
			t.Fatal(err)
		}
		// An invalid hello makes spawn reap the helper without needing protocol mocks.
		fmt.Println(string(data))
		return
	}
	t.Setenv("ZUT_L2_HOST_ONLY", "must-not-leak")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	result := filepath.Join(dir, "result.json")
	mgr := New(dir, "", "test", "", "", nil, WithEnv([]string{"ZUT_L2_EXTENSION_HELPER=1", "ZUT_L2_VALUE=session", "ZUT_L2_RESULT=" + result}), WithCWD(dir))
	ext := &Extension{Manifest: Manifest{Name: "session-helper", Exec: bin, Args: []string{"-test.run=^TestExtensionSpawnUsesSessionEnv$"}}, Dir: dir}
	err = mgr.spawn(context.Background(), ext)
	if err == nil || !strings.Contains(err.Error(), "parse hello") {
		t.Fatalf("unexpected result: %v", err)
	}
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "session" || got[1] != dir || got[2] != "" {
		t.Fatalf("child environment/cwd = %v", got)
	}
}
