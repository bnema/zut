package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/tools"
)

func testZutfileSpec(t *testing.T, cwd, pre string) *session.ZutfileSpec {
	t.Helper()
	data, skillsRoot := t.TempDir(), t.TempDir()
	dir := filepath.Join(skillsRoot, "packaged-skill")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: packaged-skill\ndescription: packaged.\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var perms tools.PermissionSet
	perms.FS.Read = []string{cwd, data}
	perms.FS.Write = []string{data}
	perms.Bash.Mode = "ask"
	return &session.ZutfileSpec{Name: "pkg-agent", DataDir: data, Permissions: &perms, SkillDirs: []string{skillsRoot}, Pre: pre}
}

func TestNewSessionDepsAppliesZutfileSpec(t *testing.T) {
	sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	cwd := t.TempDir()
	marker := filepath.Join(t.TempDir(), "pre-ran")
	z := testZutfileSpec(t, cwd, "")
	pre := "!touch " + marker
	if runtime.GOOS == "windows" {
		pre = "!type nul > " + marker
	}
	z.Pre = pre
	// Bash permission "ask" lets pre run.
	_, err := os.Stat(marker)
	if !os.IsNotExist(err) {
		t.Fatalf("marker exists before session: %v", err)
	}
	deps, _ := newTestSessionDeps(t, session.CreateSpec{CWD: cwd, Env: os.Environ(), Args: sessionAssemblyArgs(server), Zutfile: z})
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("entry.pre did not run: %v", err)
	}
	a := deps.ToolRefresher.(sessionToolRefresher).a
	if a.args.AgentName != "pkg-agent" || a.args.AgentDataDir != z.DataDir {
		t.Fatalf("agent name/data = %q/%q", a.args.AgentName, a.args.AgentDataDir)
	}
	if a.args.PermissionSet == nil || !reflect.DeepEqual(*a.args.PermissionSet, *z.Permissions) {
		t.Fatalf("permission set = %#v", a.args.PermissionSet)
	}
	if a.sandbox == nil || a.sandbox.Permissions != a.args.PermissionSet {
		t.Fatal("sandbox does not enforce the zutfile permissions")
	}
	if err := a.sandbox.CheckWritePath(filepath.Join(cwd, "x")); err == nil {
		t.Fatal("write outside declared scope allowed")
	}
	if len(a.args.ExtraSkillDirs) == 0 || a.args.ExtraSkillDirs[0] != z.SkillDirs[0] {
		t.Fatalf("extra skill dirs = %v", a.args.ExtraSkillDirs)
	}
	found := false
	for _, sk := range deps.SkillStore.SkillSnapshot() {
		found = found || sk.Name == "packaged-skill"
	}
	if !found {
		t.Fatal("packaged skill not discovered")
	}
}

func TestNewSessionDepsZutfilePreFailureFailsCreate(t *testing.T) {
	sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	cwd := t.TempDir()
	pre := "!false"
	if runtime.GOOS == "windows" {
		pre = "!exit 1"
	}
	z := testZutfileSpec(t, cwd, pre)
	_, cleanup, err := NewSessionDeps(t.Context(), session.CreateSpec{CWD: cwd, Env: os.Environ(), Args: sessionAssemblyArgs(server), Zutfile: z}, "test")
	if err == nil {
		cleanup()
		t.Fatal("expected entry.pre failure")
	}
	if !strings.Contains(err.Error(), "entry.pre") {
		t.Fatalf("error = %v", err)
	}
}

func TestCreateSpecZutfileWireRoundTrip(t *testing.T) {
	z := testZutfileSpec(t, t.TempDir(), "!echo hi")
	in := daemonproto.CreateRequest{ID: "1", Type: "create", Spec: session.CreateSpec{CWD: "/w", Args: []string{"-p"}, Zutfile: z}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out daemonproto.CreateRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch: %s", b)
	}
	// Older clients omit the field; it stays nil.
	plain, err := json.Marshal(session.CreateSpec{CWD: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "zutfile") {
		t.Fatalf("absent zutfile serialized: %s", plain)
	}
	var old session.CreateSpec
	if err := json.Unmarshal([]byte(`{"cwd":"/w","env":null,"args":null,"resume_path":"","fork":null,"headless":false}`), &old); err != nil || old.Zutfile != nil {
		t.Fatalf("old payload: %+v err=%v", old, err)
	}
}

func TestZutfileSpecWithoutPreKeepsPermissions(t *testing.T) {
	z := testZutfileSpec(t, t.TempDir(), "!echo hi")
	c := z.WithoutPre()
	if c.Pre != "" || z.Pre == "" || c.Permissions != z.Permissions || c.Name != z.Name {
		t.Fatalf("WithoutPre = %+v", c)
	}
	if (*session.ZutfileSpec)(nil).WithoutPre() != nil {
		t.Fatal("nil WithoutPre")
	}
}

func TestZutfileClientArgs(t *testing.T) {
	before := Args{AppendSystemPrompt: []string{"user"}}
	after := before
	after.Provider, after.Model = "openai", "gpt-5"
	after.AppendSystemPrompt = append(append([]string{}, before.AppendSystemPrompt...), "agent md")
	after.Prompt = "-default"
	got := zutfileClientArgs([]string{"-p"}, before, after)
	want := []string{"-p", "--provider", "openai", "--model", "gpt-5", "--append-system-prompt", "agent md", "--", "-default"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %q", got)
	}
	parsed, err := ParseArgs(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Prompt != "-default" || parsed.Model != "gpt-5" || len(parsed.AppendSystemPrompt) != 1 {
		t.Fatalf("parsed = %+v", parsed)
	}
}

// A startup extension Notify (missing --ext path) fires while the session is
// created; the pre output still reaches the creator via the create response.
func TestZutfilePreOutputReachesCreator(t *testing.T) {
	home := sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	td := startFactoryDaemon(t, home, daemonTestSocket(t))
	c := dialTestDaemon(t, td.socket)
	ctx := factoryTestCtx(t)
	cwd := t.TempDir()
	args := append(sessionAssemblyArgs(server), "--ext", filepath.Join(cwd, "missing-ext"))
	created, err := c.CreateWithOutput(ctx, session.CreateSpec{CWD: cwd, Env: os.Environ(), Args: args, Zutfile: testZutfileSpec(t, cwd, "!echo pre-client-output")})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || !strings.Contains(created.PreOutput, "pre-client-output") {
		t.Fatalf("create = %+v", created)
	}
	snap, err := c.Port(session.ID(created.ID), false).Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range snap.Notes {
		if strings.Contains(n.Text, "pre-client-output") {
			t.Fatalf("pre output leaked into snapshot notes: %+v", snap.Notes)
		}
	}
}

// The headless creator prints pre output to its injected stderr.
func TestZutfilePreOutputHeadlessStderr(t *testing.T) {
	home := sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	td := startFactoryDaemon(t, home, daemonTestSocket(t))
	cwd := t.TempDir()
	var stdout, stderr bytes.Buffer
	args := Args{CWD: cwd, zutfile: testZutfileSpec(t, cwd, "!echo pre-headless-output")}
	h, err := openHeadlessSession(factoryTestCtx(t), headlessTestIO(td.socket, &stdout, &stderr, os.Environ()), args, sessionAssemblyArgs(server))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.end(false) })
	if !strings.Contains(stderr.String(), "pre-headless-output") || stdout.Len() != 0 {
		t.Fatalf("stderr = %q stdout = %q", stderr.String(), stdout.String())
	}
}

// A prompt-turn entry.pre (no leading !) produces no shell output.
func TestZutfilePromptPreHasNoOutput(t *testing.T) {
	home := sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	td := startFactoryDaemon(t, home, daemonTestSocket(t))
	c := dialTestDaemon(t, td.socket)
	cwd := t.TempDir()
	created, err := c.CreateWithOutput(factoryTestCtx(t), session.CreateSpec{CWD: cwd, Env: os.Environ(), Args: sessionAssemblyArgs(server), Zutfile: testZutfileSpec(t, cwd, "say hello")})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.PreOutput != "" {
		t.Fatalf("create = %+v", created)
	}
}
