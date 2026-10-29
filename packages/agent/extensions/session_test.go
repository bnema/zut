package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
	session := filepath.Join(dir, "bin")
	host := t.TempDir()
	if err := os.Mkdir(session, 0700); err != nil {
		t.Fatal(err)
	}
	name := "l2-extension"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	for _, dest := range []string{session, host} {
		in, err := os.Open(bin)
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.OpenFile(filepath.Join(dest, name), os.O_CREATE|os.O_WRONLY, 0700)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(out, in)
		in.Close()
		out.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", host)
	result := filepath.Join(dir, "result.json")
	mgr := New(dir, "", "test", "", "", nil, WithEnv([]string{"PATH=bin", "ZUT_L2_EXTENSION_HELPER=1", "ZUT_L2_VALUE=session", "ZUT_L2_RESULT=" + result}), WithCWD(dir))
	ext := &Extension{Manifest: Manifest{Name: "session-helper", Exec: name, Args: []string{"-test.run=^TestExtensionSpawnUsesSessionEnv$"}}, Dir: dir}
	err = mgr.spawn(context.Background(), ext)
	if err == nil || !strings.Contains(err.Error(), "parse hello") {
		t.Fatalf("unexpected result: %v", err)
	}
	if ext.cmd.Path != filepath.Join(session, name) {
		t.Fatalf("executable = %q", ext.cmd.Path)
	}
	empty := New(dir, "", "test", "", "", nil, WithEnv([]string{}), WithCWD(dir))
	if err := empty.spawn(context.Background(), &Extension{Manifest: Manifest{Name: "empty", Exec: name}, Dir: dir}); err == nil {
		t.Fatal("empty env used host PATH")
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
