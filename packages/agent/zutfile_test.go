package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/tui"
)

func TestRunZutfileOrchestrationRejectsPackagedAgentBeforeSideEffects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	// A nonexistent reference proves validation happens before local loading,
	// remote fetching, unpacking, data-directory creation, or consent.
	missingPackage := filepath.Join(t.TempDir(), "missing.zut")

	handled, err := runZutfileCommand([]string{"run", missingPackage, "--print", "--orchestrate", "task"}, "test")
	if !handled {
		t.Fatal("run command was not handled")
	}
	if err == nil || !strings.Contains(err.Error(), "packaged agents") {
		t.Fatalf("run error = %v, want packaged-agent validation error", err)
	}
	entries, readErr := os.ReadDir(home)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("packaged-agent rejection created state: %v", entries)
	}
}

func TestReadZutfileConsentKeyAcceptsOnlyYesNo(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  byte
	}{
		{name: "lowercase yes", input: []byte{'y', '\r'}, want: 'y'},
		{name: "uppercase yes", input: []byte{'Y', '\n'}, want: 'y'},
		{name: "lowercase no", input: []byte{'n', '\r'}, want: 'n'},
		{name: "uppercase no", input: []byte{'N', '\n'}, want: 'n'},
		{name: "ignore unrelated keys", input: append([]byte("x\r\n\x1b[A"), 'y', '\r'), want: 'y'},
		{name: "keep first valid answer", input: []byte{'n', 'y', '\r'}, want: 'n'},
		{name: "backspace changes answer", input: []byte{'y', 0x7f, 'n', '\r'}, want: 'n'},
		{name: "ctrl h changes answer", input: []byte{'n', '\b', 'y', '\r'}, want: 'y'},
		{name: "backspace without answer is ignored", input: []byte{0x7f, 'y', '\r'}, want: 'y'},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := readZutfileConsentKey(bytes.NewReader(tt.input), func(answer byte) {
				if answer == 0 {
					out.Truncate(out.Len() - 1)
					return
				}
				out.WriteByte(answer)
			})
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("answer = %q, want %q", got, tt.want)
			}
			if out.String() != string(tt.want) {
				t.Fatalf("echo = %q, want %q", out.String(), tt.want)
			}
		})
	}
}

func TestReadZutfileConsentKeyWaitsForEnter(t *testing.T) {
	var out bytes.Buffer
	if _, err := readZutfileConsentKey(strings.NewReader("y"), func(answer byte) {
		out.WriteByte(answer)
	}); !errors.Is(err, io.EOF) {
		t.Fatalf("error = %v, want EOF before Enter", err)
	}
	if out.String() != "y" {
		t.Fatalf("echo = %q, want y", out.String())
	}
}

func TestReadZutfileConsentKeyInterruptsOnCtrlC(t *testing.T) {
	if _, err := readZutfileConsentKey(bytes.NewReader([]byte{'x', 3, 'y'}), nil); !errors.Is(err, errZutfileConsentInterrupted) {
		t.Fatalf("error = %v, want interrupted", err)
	}
}

func TestReadZutfileConsentKeyReportsInputError(t *testing.T) {
	if _, err := readZutfileConsentKey(strings.NewReader("xxx"), nil); !errors.Is(err, io.EOF) {
		t.Fatalf("error = %v, want EOF", err)
	}
}

func writeTestZutfile(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("Be useful."), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadZutfileAllowsMissingAgentMD(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"zutfile":1,"name":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	zf, cleanup, err := loadZutfile(dir)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("load without AGENT.md: %v", err)
	}
	if zf.Manifest.Name != "test" {
		t.Fatalf("name = %q, want test", zf.Manifest.Name)
	}
}

func TestValidateZutfileDirRejectsAgentMDDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "AGENT.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateZutfileDir(dir); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadZutfileRejectsUnenforcedPermissions(t *testing.T) {
	for _, field := range []string{
		`"net":{"allow":["example.com"]}`,
		`"env":{"read":["HOME"]}`,
	} {
		dir := writeTestZutfile(t, `{"zutfile":1,"name":"test","permissions":{`+field+`}}`)
		if _, _, err := loadZutfile(dir); err == nil {
			t.Fatalf("manifest with %s was accepted", field)
		}
	}
}

func TestLoadZutfileBashModes(t *testing.T) {
	for _, mode := range []string{"none", "ask", "allowlist"} {
		manifest := `{"zutfile":1,"name":"test","permissions":{"bash":{"mode":"` + mode + `"}}}`
		if mode == "allowlist" {
			manifest = `{"zutfile":1,"name":"test","permissions":{"bash":{"mode":"allowlist","allow":["git"]}}}`
		}
		dir := writeTestZutfile(t, manifest)
		if _, _, err := loadZutfile(dir); err != nil {
			t.Fatalf("mode %q rejected: %v", mode, err)
		}
	}
	dir := writeTestZutfile(t, `{"zutfile":1,"name":"test","permissions":{"bash":{"mode":"all"}}}`)
	if _, _, err := loadZutfile(dir); err == nil || !strings.Contains(err.Error(), "unsupported bash permission mode") {
		t.Fatalf("unexpected error for all mode: %v", err)
	}
	dir = writeTestZutfile(t, `{"zutfile":1,"name":"test","permissions":{"bash":{"mode":"everything"}}}`)
	if _, _, err := loadZutfile(dir); err == nil || !strings.Contains(err.Error(), "unsupported bash permission mode") {
		t.Fatalf("unexpected error for unknown mode: %v", err)
	}
}

func TestLoadZutfileRejectsUnsafeOrCollidingNames(t *testing.T) {
	for _, name := range []string{"...", "Name", "two words", "a/b"} {
		dir := writeTestZutfile(t, `{"zutfile":1,"name":"`+name+`"}`)
		if _, _, err := loadZutfile(dir); err == nil {
			t.Fatalf("unsafe manifest name %q was accepted", name)
		}
	}
}

func TestResolveZutfileRefUsesLocalFirstThenGitHubShorthand(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Mkdir("local-agent", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("packed-agent.zut", []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		ref  string
		want string
	}{
		{"local-agent", "local-agent"},
		{"packed-agent", "packed-agent.zut"},
		{"remote-agent", "remote-agent"},
		{"missing.zut", "missing.zut"},
		{"agents/remote-agent", "https://github.com/agents/remote-agent"},
		{`agents\remote-agent`, "https://github.com/agents/remote-agent"},
		{"frkr/zot-archify", "https://github.com/frkr/zot-archify"},
		{"acme/agents/reviewer", "https://github.com/acme/agents/reviewer"},
		{`acme\agents\reviewers\go`, "https://github.com/acme/agents/reviewers/go"},
		{"./missing-agent", "./missing-agent"},
		{"Remote-Agent", "Remote-Agent"},
		{"https://github.com/acme/agents/example", "https://github.com/acme/agents/example"},
	}
	for _, tt := range tests {
		got, err := resolveZutfileRef(tt.ref)
		if err != nil {
			t.Fatalf("resolve %q: %v", tt.ref, err)
		}
		if got != tt.want {
			t.Errorf("resolve %q = %q, want %q", tt.ref, got, tt.want)
		}
	}
}

func TestLoadZutfileRejectsBundledExecutableExtension(t *testing.T) {
	dir := writeTestZutfile(t, `{"zutfile":1,"name":"test"}`)
	ext := filepath.Join(dir, "extensions", "bad")
	if err := os.MkdirAll(ext, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ext, "extension.json"), []byte(`{"name":"bad"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadZutfile(dir); err == nil || !strings.Contains(err.Error(), "cannot yet be confined") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckZutfileMinVersion(t *testing.T) {
	var zf zutfileLoaded
	zf.Manifest.Runtime.MinZut = "0.3.0"
	if err := checkZutfileRequirements(zf, "0.2.75"); err == nil {
		t.Fatal("old zut version accepted")
	}
	if err := checkZutfileRequirements(zf, "0.3.0"); err != nil {
		t.Fatalf("minimum version rejected: %v", err)
	}
}

func TestApplyZutfileModelRequirementsRejectsUnsupportedFields(t *testing.T) {
	var m ZutfileManifest
	m.Model.MinTier = "frontier"
	if err := applyZutfileModelRequirements(&Args{}, m); err == nil {
		t.Fatal("unsupported min_tier was ignored")
	}
	m.Model.MinTier = ""
	m.Model.Requires = []string{"audio"}
	if err := applyZutfileModelRequirements(&Args{}, m); err == nil {
		t.Fatal("unsupported capability was ignored")
	}
}

func TestUntarRejectsTraversalAndOversizedEntry(t *testing.T) {
	makeTar := func(name string, size int64) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size}); err != nil {
			t.Fatal(err)
		}
		_ = tw.Close()
		return buf.Bytes()
	}
	if err := untar(bytes.NewReader(makeTar("../escape", 0)), t.TempDir()); err == nil {
		t.Fatal("path traversal accepted")
	}
	if err := untar(bytes.NewReader(makeTar("large", maxZutfileEntrySize+1)), t.TempDir()); err == nil {
		t.Fatal("oversized entry accepted")
	}
}

func TestParseGitHubAgentURL(t *testing.T) {
	tests := []struct {
		input                 string
		owner, repo, ref, dir string
	}{
		{"https://github.com/acme/agents/reviewer", "acme", "agents", "HEAD", "reviewer"},
		{"https://github.com/acme/agents/tree/main/reviewer", "acme", "agents", "main", "reviewer"},
		{"https://github.com/acme/agents/reviewers/go", "acme", "agents", "HEAD", "reviewers/go"},
		{"https://github.com/acme/agent.git", "acme", "agent", "HEAD", ""},
	}
	for _, tt := range tests {
		u, err := url.Parse(tt.input)
		if err != nil {
			t.Fatal(err)
		}
		owner, repo, ref, dir, err := parseGitHubAgentURL(u)
		if err != nil {
			t.Fatalf("parse %s: %v", tt.input, err)
		}
		if owner != tt.owner || repo != tt.repo || ref != tt.ref || dir != tt.dir {
			t.Fatalf("parse %s = %q, %q, %q, %q", tt.input, owner, repo, ref, dir)
		}
	}
}

func TestLoadRemoteZutfileDownloadsTemporaryGitHubArchive(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"agents-main/zut-maintenance/manifest.json": `{"zutfile":1,"name":"zut-maintenance"}`,
		"agents-main/zut-maintenance/AGENT.md":      "Maintain zut.",
	}
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(archive.Bytes())
	}))
	defer server.Close()
	oldArchiveURL := githubArchiveURL
	githubArchiveURL = func(_, _, _ string) string { return server.URL }
	t.Cleanup(func() { githubArchiveURL = oldArchiveURL })

	u, _ := url.Parse("https://github.com/acme/agents/zut-maintenance")
	zf, cleanup, err := loadRemoteZutfile(u)
	if err != nil {
		t.Fatal(err)
	}
	if zf.Manifest.Name != "zut-maintenance" || !zf.Temp {
		t.Fatalf("unexpected zutfile: %+v", zf)
	}
	root := filepath.Dir(filepath.Dir(zf.Dir))
	cleanup()
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("temporary checkout was not removed: %v", err)
	}
}

func TestFormatZutfileConsentUsesThemeColors(t *testing.T) {
	th := tui.Theme{
		FG: tui.Color256(1), Muted: tui.Color256(2), Accent: tui.Color256(3),
		Assistant: tui.Color256(4), Warning: tui.Color256(5),
	}
	zf := zutfileLoaded{}
	zf.Manifest.Name = "reviewer"
	zf.Manifest.Version = "1.2.3"
	perms := tools.PermissionSet{}
	perms.FS.Read = []string{"/workspace"}
	perms.Bash.Mode = "ask"

	got := formatZutfileConsent(zf, perms, th, true)
	for _, want := range []string{
		th.FGColor(th.Assistant, "Agent"),
		th.FGColor(th.Accent, "reviewer@1.2.3"),
		th.FGColor(th.Muted, "  fs read: "),
		th.FGColor(th.FG, "/workspace"),
		th.FGColor(th.Warning, "ask"),
		th.FGColor(th.Assistant, "Allow?"),
		th.FGColor(th.Muted, "[y/n]"),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("themed consent missing %q:\n%s", want, got)
		}
	}
}

func TestPermissionSummaryShowsDeniedScopes(t *testing.T) {
	got := permissionSummary(tools.PermissionSet{})
	if !strings.Contains(got, "fs read: none") || !strings.Contains(got, "fs write: none") {
		t.Fatalf("summary did not show denied scopes:\n%s", got)
	}
}

func TestPeelLeadingYes(t *testing.T) {
	yes, rest := peelLeadingYes([]string{"-y", "--yes", "agent", "-y"})
	if !yes {
		t.Fatal("expected yes")
	}
	if len(rest) != 2 || rest[0] != "agent" || rest[1] != "-y" {
		t.Fatalf("rest = %v", rest)
	}
	yes, rest = peelLeadingYes([]string{"agent", "-y"})
	if yes || len(rest) != 2 {
		t.Fatalf("yes=%v rest=%v", yes, rest)
	}
}

func TestConsentZutfileAutoYesWritesReceipt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	zf := zutfileLoaded{Digest: "abc123", Manifest: ZutfileManifest{Name: "demo", Version: "1.0.0"}}
	var perms tools.PermissionSet
	perms.Bash.Mode = "none"
	allowed, err := consentZutfile(zf, perms, true)
	if err != nil || !allowed {
		t.Fatalf("auto yes: allowed=%v err=%v", allowed, err)
	}
	path := filepath.Join(home, "agents", "demo", "consents", "abc123.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("consent receipt missing: %v", err)
	}
	// Second call without -y should use the receipt.
	allowed, err = consentZutfile(zf, perms, false)
	if err != nil || !allowed {
		t.Fatalf("cached consent: allowed=%v err=%v", allowed, err)
	}
}

func TestConsentZutfileAutoYesAskDoesNotCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	zf := zutfileLoaded{Digest: "askdig", Manifest: ZutfileManifest{Name: "ask-agent", Version: "1.0.0"}}
	var perms tools.PermissionSet
	perms.Bash.Mode = "ask"
	if _, err := consentZutfile(zf, perms, true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "agents", "ask-agent", "consents", "askdig.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ask mode should not cache consent, err=%v", err)
	}
}
