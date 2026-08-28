package extensions

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/bnema/zut/packages/agent/extproto"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLifecycleWriteDropsWhenSynchronousWriterOwnsLock(t *testing.T) {
	ext := &Extension{stdin: &nopWriteCloser{}}
	ext.writeMu.Lock()
	defer ext.writeMu.Unlock()

	done := make(chan error, 1)
	go func() { done <- ext.tryWriteLifecycleFrame([]byte("lifecycle")) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("lifecycle write unexpectedly acquired the synchronous writer lock")
		}
	case <-time.After(lifecycleWriteGrace + time.Second):
		t.Fatal("lifecycle write remained blocked on the synchronous writer lock")
	}
}

type nopWriteCloser struct{}

func (*nopWriteCloser) Write(data []byte) (int, error) { return len(data), nil }
func (*nopWriteCloser) Close() error                   { return nil }

var _ io.WriteCloser = (*nopWriteCloser)(nil)

// stubHooks records every callback so the test can assert on them.
type stubHooks struct {
	mu           sync.Mutex
	notifies     []string
	displays     []string
	alerts       []extproto.AlertRequest
	alertExts    []string
	submits      []string
	submitSlash  []string
	clearNotes   []string
	panels       []extproto.PanelSpec
	panelExts    []string
	chromeClears []string
}

func (s *stubHooks) Notify(name, level, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifies = append(s.notifies, name+":"+level+":"+message)
}
func (s *stubHooks) Alert(name string, alert extproto.AlertRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alertExts = append(s.alertExts, name)
	s.alerts = append(s.alerts, alert)
}
func (s *stubHooks) Submit(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.submits = append(s.submits, text)
}
func (s *stubHooks) SubmitSlash(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.submitSlash = append(s.submitSlash, text)
}
func (s *stubHooks) Insert(string) {}
func (s *stubHooks) Display(name, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.displays = append(s.displays, name+":"+text)
}
func (s *stubHooks) ClearNotes(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearNotes = append(s.clearNotes, name)
}
func (s *stubHooks) OpenPanel(extName string, spec extproto.PanelSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.panelExts = append(s.panelExts, extName)
	s.panels = append(s.panels, spec)
}
func (s *stubHooks) UpdatePanel(string, string, string, []string, string) {}
func (s *stubHooks) ClosePanel(string, string)                            {}
func (s *stubHooks) SetStatus(string, string, string, string)             {}
func (s *stubHooks) SetWidget(string, string, string, string, []string)   {}
func (s *stubHooks) ClearWidget(string, string)                           {}
func (s *stubHooks) ClearExtensionChrome(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chromeClears = append(s.chromeClears, name)
}

func TestDiscoverIgnoresDirectoriesWithoutManifest(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()

	orphanedDataDir := filepath.Join(cwd, ".zut", "extensions", "example", "projects")
	if err := os.MkdirAll(orphanedDataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanedDataDir, "state.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	globalExtDir := filepath.Join(home, "extensions", "example")
	if err := os.MkdirAll(globalExtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalExtDir, "extension.json"), []byte(`{"name":"example"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalExtDir, "theme.json"), []byte(`{"name":"Example"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := New(home, cwd, "0.0.0-test", "", "", nil)
	if errs := mgr.Discover(context.Background()); len(errs) > 0 {
		t.Fatalf("discover errors = %v, want none", errs)
	}
	if _, ok := mgr.ext["example"]; !ok {
		t.Fatal("global extension was shadowed by a project directory without a manifest")
	}
}

// writeMockExtension creates a minimal extension on disk that uses a
// shell script (or batch file on windows) to drive the protocol. The
// script reads commands from stdin and emits hard-coded responses,
// exercising the manager's spawn/handshake/dispatch path without
// needing the SDK.
func writeMockExtension(t *testing.T, root string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mock extension uses /bin/sh; skip on windows")
	}

	dir := filepath.Join(root, "mock")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Shell script: emit hello, read frames, respond. Reads until
	// stdin closes; tail's -F keeps the pipe alive long enough for
	// the manager to send command_invoked.
	script := `#!/bin/sh
printf '%s\n' '{"type":"hello","protocol_version":2,"name":"mock","version":"0.0.1","capabilities":["commands"]}'
printf '%s\n' '{"type":"register_command","name":"Ping","description":"ping/pong"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"command_invoked"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
      case "$line" in
        *'"name":"Ping"'*)
          printf '%s\n' "{\"type\":\"command_response\",\"id\":\"$id\",\"action\":\"display\",\"display\":\"pong\"}"
          ;;
        *)
          printf '%s\n' "{\"type\":\"command_response\",\"id\":\"$id\",\"error\":\"non-canonical command name\"}"
          ;;
      esac
      ;;
    *'"type":"shutdown"'*)
      printf '%s\n' '{"type":"shutdown_ack"}'
      exit 0
      ;;
  esac
done
`
	scriptPath := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := map[string]any{
		"name":    "mock",
		"version": "0.0.1",
		"exec":    "./run.sh",
	}
	mfb, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "extension.json"), mfb, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverReportsExitStatusAndLogWhenExtensionExitsBeforeHello(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock extension uses /bin/sh; skip on windows")
	}

	tmp := t.TempDir()
	extDir := filepath.Join(tmp, "extensions", "broken")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' 'compile failed' >&2\nexit 23\n"
	if err := os.WriteFile(filepath.Join(extDir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "extension.json"), []byte(`{"name":"broken","exec":"./run.sh"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := New(tmp, "", "0.0.0-test", "", "", nil)
	errs := mgr.Discover(context.Background())
	if len(errs) != 1 {
		t.Fatalf("discover errors = %v, want one", errs)
	}
	logPath := filepath.Join(tmp, "logs", "ext-broken.log")
	got := errs[0].Error()
	if strings.Contains(got, "%!w") {
		t.Fatalf("error contains formatting artifact: %s", got)
	}
	if !strings.Contains(got, "extension exited before hello: exit status 23") || !strings.Contains(got, logPath) {
		t.Fatalf("error does not report exit status and identify stderr log: %s", got)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read extension log: %v", err)
	}
	if !strings.Contains(string(logData), "compile failed") {
		t.Fatalf("extension stderr missing from log: %s", logData)
	}
}

func TestDiscoverReportsSignalWhenExtensionExitsBeforeHello(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock extension uses Unix signals; skip on windows")
	}

	tmp := t.TempDir()
	extDir := filepath.Join(tmp, "extensions", "signaled")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nkill -TERM $$\n"
	if err := os.WriteFile(filepath.Join(extDir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "extension.json"), []byte(`{"name":"signaled","exec":"./run.sh"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := New(tmp, "", "0.0.0-test", "", "", nil)
	errs := mgr.Discover(context.Background())
	if len(errs) != 1 {
		t.Fatalf("discover errors = %v, want one", errs)
	}
	got := errs[0].Error()
	if !strings.Contains(got, "extension exited before hello: signal:") || !strings.Contains(got, "ext-signaled.log") {
		t.Fatalf("error does not report signal and identify stderr log: %s", got)
	}
}

func TestSpawnCleansUpProcessAfterInvalidHello(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock extension uses /bin/sh; skip on windows")
	}

	tmp := t.TempDir()
	extDir := filepath.Join(tmp, "broken")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(extDir, "run.sh")
	script := "#!/bin/sh\nprintf '%s\\n' 'not-json'\nwhile IFS= read -r line; do :; done\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ext := &Extension{Manifest: Manifest{Name: "broken", Exec: "./run.sh"}, Dir: extDir}
	mgr := New(tmp, "", "0.0.0-test", "", "", nil)
	err := mgr.spawn(context.Background(), ext)
	if err == nil || !strings.Contains(err.Error(), "parse hello") {
		t.Fatalf("spawn error = %v, want parse hello failure", err)
	}
	if ext.cmd == nil || ext.cmd.ProcessState == nil {
		t.Fatal("failed handshake process was not reaped")
	}
	if _, err := ext.logFile.WriteString("after cleanup"); err == nil {
		t.Fatal("failed handshake log file was not closed")
	}
}

func TestSpawnRejectsUnsupportedExtensionProtocolVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock extension uses /bin/sh; skip on windows")
	}

	tmp := t.TempDir()
	extDir := filepath.Join(tmp, "broken")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' '{"type":"hello","protocol_version":1,"name":"broken"}'
while IFS= read -r line; do :; done
`
	if err := os.WriteFile(filepath.Join(extDir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ext := &Extension{Manifest: Manifest{Name: "broken", Exec: "./run.sh"}, Dir: extDir}
	mgr := New(tmp, "", "0.0.0-test", "", "", nil)
	err := mgr.spawn(context.Background(), ext)
	if err == nil || !strings.Contains(err.Error(), "unsupported extension protocol version 1") {
		t.Fatalf("spawn error = %v", err)
	}
	if ext.cmd == nil || ext.cmd.ProcessState == nil {
		t.Fatal("failed protocol handshake process was not reaped")
	}
}

func TestDiscoverLoadsThemeOnlyExtension(t *testing.T) {
	tmp := t.TempDir()
	extDir := filepath.Join(tmp, "extensions", "theme-only")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"theme-only","version":"1.0.0","description":"theme only"}`
	if err := os.WriteFile(filepath.Join(extDir, "extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	theme := `{"name":"Theme Only","description":"theme from extension","colors":{"dark":{"accent":204}}}`
	if err := os.WriteFile(filepath.Join(extDir, "theme.json"), []byte(theme), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := New(tmp, "", "0.0.0-test", "anthropic", "claude-opus-4-7", nil)
	if errs := mgr.Discover(context.Background()); len(errs) > 0 {
		t.Fatalf("discover errors: %v", errs)
	}
	defer mgr.Stop(10 * time.Millisecond)

	opts := mgr.ThemeOptions()
	if len(opts) != 1 {
		t.Fatalf("theme options = %d, want 1", len(opts))
	}
	if opts[0].Label != "Theme Only" || opts[0].Path != filepath.Join(extDir, "theme.json") {
		t.Fatalf("unexpected theme option: %#v", opts[0])
	}
	if !strings.Contains(opts[0].Description, "from extension theme-only") {
		t.Fatalf("description missing extension source: %q", opts[0].Description)
	}
}

func TestManifestBundlesSkillsWithSafePrecedence(t *testing.T) {
	tmp := t.TempDir()
	extRoot := filepath.Join(tmp, "extensions")
	extDir := filepath.Join(extRoot, "bundle")
	if err := os.MkdirAll(filepath.Join(extDir, "skills", "phases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "skills", "phases", "SKILL.md"), []byte("---\nname: phases\ndescription: bundled phases\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"bundle","skills":["skills"],"description":"bundle"}`
	if err := os.WriteFile(filepath.Join(extDir, "extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "theme.json"), []byte(`{"name":"bundle","colors":{"dark":{"accent":1}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := New(tmp, "", "0.0.0-test", "", "", &stubHooks{})
	if errs := mgr.Discover(context.Background()); len(errs) > 0 {
		t.Fatalf("discover errors: %v", errs)
	}
	defer mgr.Stop(10 * time.Millisecond)
	bundled := mgr.Skills()
	if len(bundled) != 1 || bundled[0].Name != "phases" || bundled[0].Source != "extension bundle" {
		t.Fatalf("bundled skills = %#v", bundled)
	}
	if _, errs := loadManifestSkills(extDir, Manifest{Name: "bundle", Skills: []string{"../outside"}}); len(errs) == 0 {
		t.Fatal("path traversal manifest was accepted")
	}
	for _, path := range []string{"", filepath.Join(string(filepath.Separator), "outside")} {
		if _, errs := loadManifestSkills(extDir, Manifest{Name: "bundle", Skills: []string{path}}); len(errs) == 0 {
			t.Fatalf("unsafe skill path %q was accepted", path)
		}
	}
	if runtime.GOOS != "windows" {
		outside := filepath.Join(tmp, "outside", "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outside, []byte("---\nname: escape\ndescription: outside\n---\nbody\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		unsafeDir := filepath.Join(extDir, "unsafe")
		if err := os.MkdirAll(unsafeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(unsafeDir, "SKILL.md")); err != nil {
			t.Fatal(err)
		}
		loaded, errs := loadManifestSkills(extDir, Manifest{Name: "bundle", Skills: []string{"unsafe"}})
		if len(errs) == 0 || len(loaded) != 0 {
			t.Fatalf("symlinked skill outside extension was accepted: skills=%#v errors=%v", loaded, errs)
		}
	}
}

func TestManagerSpawnAndInvoke(t *testing.T) {
	tmp := t.TempDir()
	extRoot := filepath.Join(tmp, "extensions")
	if err := os.MkdirAll(extRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMockExtension(t, extRoot)

	hooks := &stubHooks{}
	mgr := New(tmp, "", "0.0.0-test", "anthropic", "claude-opus-4-7", hooks)

	if errs := mgr.Discover(context.Background()); len(errs) > 0 {
		t.Fatalf("discover errors: %v", errs)
	}
	defer mgr.Stop(2 * time.Second)

	// Give the extension a beat to send register_command frames after
	// the hello handshake.
	time.Sleep(150 * time.Millisecond)

	cmds := mgr.Commands()
	found := false
	for _, c := range cmds {
		if c.Name == "Ping" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected canonical command 'Ping', got %#v", cmds)
	}
	if !mgr.HasCommand("PING") {
		t.Fatal("HasCommand(\"PING\") = false")
	}
	if owner := mgr.CommandOwner("pInG"); owner != "mock" {
		t.Fatalf("CommandOwner(\"pInG\") = %q, want mock", owner)
	}

	resp, err := mgr.Invoke(context.Background(), "PING", "", 2*time.Second)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if resp.Action != "display" {
		t.Errorf("expected action=display, got %q", resp.Action)
	}
	if resp.Display != "pong" {
		t.Errorf("expected display=pong, got %q", resp.Display)
	}
}

func TestDiagnosticsReportMalformedFramesAndConflicts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock extension uses /bin/sh; skip on windows")
	}

	tmp := t.TempDir()
	extRoot := filepath.Join(tmp, "extensions")
	writeDiagExtension := func(name, script string) {
		t.Helper()
		dir := filepath.Join(extRoot, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		mfb, _ := json.Marshal(map[string]any{"name": name, "exec": "./run.sh"})
		if err := os.WriteFile(filepath.Join(dir, "extension.json"), mfb, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeDiagExtension("a-first", `#!/bin/sh
printf '%s\n' '{"type":"hello","protocol_version":2,"name":"a-first","version":"0.1","capabilities":["commands","tools"]}'
printf '%s\n' '{"type":"register_command","name":"CaseTest","description":"first"}'
printf '%s\n' '{"type":"register_tool","name":"shared","description":"first","schema":{"type":"object"}}'
printf '%s\n' '{"type":"register_tool","name":"broken","description":"bad","schema":'
printf '%s\n' '{"type":"ready"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"shutdown"'*) exit 0 ;;
  esac
done
`)
	writeDiagExtension("b-second", `#!/bin/sh
printf '%s\n' '{"type":"hello","protocol_version":2,"name":"b-second","version":"0.1","capabilities":["commands","tools"]}'
printf '%s\n' '{"type":"register_command","name":"casetest","description":"second"}'
printf '%s\n' '{"type":"register_tool","name":"shared","description":"second","schema":{"type":"object"}}'
printf '%s\n' '{"type":"ready"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"shutdown"'*) exit 0 ;;
  esac
done
`)

	mgr := New(tmp, "", "0.0.0-test", "anthropic", "claude-opus-4-7", &stubHooks{})
	if errs := mgr.Discover(context.Background()); len(errs) > 0 {
		t.Fatalf("discover errors: %v", errs)
	}
	defer mgr.Stop(2 * time.Second)
	mgr.WaitForReady(2 * time.Second)

	diags := mgr.Diagnostics()
	byName := map[string]ExtensionDiagnostic{}
	for _, d := range diags {
		byName[d.Name] = d
	}

	first := byName["a-first"]
	if len(first.Messages) == 0 || !strings.Contains(strings.Join(first.Messages, "\n"), "malformed json frame") {
		t.Fatalf("expected malformed-frame diagnostic, got %#v", first.Messages)
	}

	var shadowedTool bool
	var activeCommands, shadowedCommands int
	var conflictMessage bool
	for _, d := range diags {
		if strings.Contains(strings.Join(d.Messages, "\n"), "conflicts with another extension") {
			conflictMessage = true
		}
		for _, tool := range d.Tools {
			if tool.Name == "shared" && !tool.Active {
				shadowedTool = true
			}
		}
		for _, command := range d.Commands {
			if !strings.EqualFold(command.Name, "casetest") {
				continue
			}
			if command.Active {
				activeCommands++
			} else {
				shadowedCommands++
			}
		}
	}
	if !shadowedTool {
		t.Fatalf("expected one shared tool registration to be inactive, got %#v", diags)
	}
	if activeCommands != 1 || shadowedCommands != 1 {
		t.Fatalf("case-only command registrations: active=%d shadowed=%d; diagnostics=%#v", activeCommands, shadowedCommands, diags)
	}
	if !conflictMessage {
		t.Fatalf("expected conflict diagnostic, got %#v", diags)
	}
}

// TestSpontaneousSubmit verifies that an extension can submit a
// non-empty prompt outside of any command response.
func TestSpontaneousSubmit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock extension uses /bin/sh; skip on windows")
	}

	tmp := t.TempDir()
	extDir := filepath.Join(tmp, "extensions", "submit-mock")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}

	script := `#!/bin/sh
printf '%s\n' '{"type":"hello","protocol_version":2,"name":"submit-mock","version":"0.1","capabilities":["submit"]}'
printf '%s\n' '{"type":"ready"}'
printf '%s\n' '{"type":"submit","text":"  explain this repository briefly  "}'
printf '%s\n' '{"type":"submit","text":"   "}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"shutdown"'*)
      printf '%s\n' '{"type":"shutdown_ack"}'
      exit 0
      ;;
  esac
done
`
	if err := os.WriteFile(filepath.Join(extDir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	mfb, _ := json.Marshal(map[string]any{"name": "submit-mock", "exec": "./run.sh"})
	if err := os.WriteFile(filepath.Join(extDir, "extension.json"), mfb, 0o644); err != nil {
		t.Fatal(err)
	}

	hooks := &stubHooks{}
	mgr := New(tmp, "", "0.0.0-test", "anthropic", "claude-opus-4-7", hooks)
	if errs := mgr.Discover(context.Background()); len(errs) > 0 {
		t.Fatalf("discover errors: %v", errs)
	}
	defer mgr.Stop(2 * time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hooks.mu.Lock()
		n := len(hooks.submits)
		hooks.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if len(hooks.submits) != 1 {
		t.Fatalf("Submit calls = %#v, want one non-empty call", hooks.submits)
	}
	if hooks.submits[0] != "explain this repository briefly" {
		t.Fatalf("Submit text = %q", hooks.submits[0])
	}
}

// TestSpontaneousAlert verifies that an extension sending an alert frame
// causes the manager to call hooks.Alert with the structured request.
func TestSpontaneousAlert(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock extension uses /bin/sh; skip on windows")
	}

	tmp := t.TempDir()
	extDir := filepath.Join(tmp, "extensions", "alert-mock")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}

	script := `#!/bin/sh
printf '%s\n' '{"type":"hello","protocol_version":2,"name":"alert-mock","version":"0.1","capabilities":["alerts"]}'
printf '%s\n' '{"type":"ready"}'
printf '%s\n' '{"type":"alert","kind":"bell","reason":"question_ready"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"shutdown"'*)
      printf '%s\n' '{"type":"shutdown_ack"}'
      exit 0
      ;;
  esac
done
`
	if err := os.WriteFile(filepath.Join(extDir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(map[string]any{"name": "alert-mock", "exec": "./run.sh"})
	if err := os.WriteFile(filepath.Join(extDir, "extension.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}

	hooks := &stubHooks{}
	mgr := New(tmp, "", "0.0.0-test", "anthropic", "claude-test", hooks)
	if errs := mgr.Discover(context.Background()); len(errs) > 0 {
		t.Fatalf("discover errors: %v", errs)
	}
	defer mgr.Stop(2 * time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hooks.mu.Lock()
		n := len(hooks.alerts)
		hooks.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if len(hooks.alerts) != 1 || len(hooks.alertExts) != 1 {
		t.Fatalf("alerts = %#v, extensions = %#v; want one alert", hooks.alerts, hooks.alertExts)
	}
	if hooks.alertExts[0] != "alert-mock" {
		t.Fatalf("alert extension = %q, want alert-mock", hooks.alertExts[0])
	}
	if got := hooks.alerts[0]; got.Kind != extproto.AlertKindBell || got.Reason != "question_ready" {
		t.Fatalf("alert = %+v, want bell/question_ready", got)
	}
}

// TestSpontaneousOpenPanel verifies that an extension sending an
// open_panel frame outside of any command response causes the manager
// to call hooks.OpenPanel with the correct PanelSpec fields.
func TestSpontaneousOpenPanel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock extension uses /bin/sh; skip on windows")
	}

	tmp := t.TempDir()
	extDir := filepath.Join(tmp, "extensions", "panel-mock")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Extension emits hello + ready, then immediately fires a
	// spontaneous open_panel, then waits for shutdown.
	script := `#!/bin/sh
printf '%s\n' '{"type":"hello","protocol_version":2,"name":"panel-mock","version":"0.1","capabilities":["panels"]}'
printf '%s\n' '{"type":"ready"}'
printf '%s\n' '{"type":"open_panel","panel":{"id":"test-panel","title":"Hello Panel","lines":["line one","line two"],"footer":"esc close"}}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"shutdown"'*)
      printf '%s\n' '{"type":"shutdown_ack"}'
      exit 0
      ;;
  esac
done
`
	if err := os.WriteFile(filepath.Join(extDir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	mfb, _ := json.Marshal(map[string]any{"name": "panel-mock", "exec": "./run.sh"})
	if err := os.WriteFile(filepath.Join(extDir, "extension.json"), mfb, 0o644); err != nil {
		t.Fatal(err)
	}

	hooks := &stubHooks{}
	mgr := New(tmp, "", "0.0.0-test", "anthropic", "claude-opus-4-7", hooks)
	if errs := mgr.Discover(context.Background()); len(errs) > 0 {
		t.Fatalf("discover errors: %v", errs)
	}
	defer mgr.Stop(2 * time.Second)

	// Give the extension time to flush its open_panel frame.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hooks.mu.Lock()
		n := len(hooks.panels)
		hooks.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	hooks.mu.Lock()
	defer hooks.mu.Unlock()

	if len(hooks.panels) == 0 {
		t.Fatal("hooks.OpenPanel was never called")
	}
	spec := hooks.panels[0]
	if spec.ID != "test-panel" {
		t.Errorf("panel id: want %q, got %q", "test-panel", spec.ID)
	}
	if spec.Title != "Hello Panel" {
		t.Errorf("panel title: want %q, got %q", "Hello Panel", spec.Title)
	}
	if len(spec.Lines) != 2 || spec.Lines[0] != "line one" || spec.Lines[1] != "line two" {
		t.Errorf("panel lines: want [line one line two], got %v", spec.Lines)
	}
	if spec.Footer != "esc close" {
		t.Errorf("panel footer: want %q, got %q", "esc close", spec.Footer)
	}
	if hooks.panelExts[0] != "panel-mock" {
		t.Errorf("ext name: want %q, got %q", "panel-mock", hooks.panelExts[0])
	}
}
