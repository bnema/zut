package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/auth"
	"github.com/bnema/zut/packages/tui"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/term"
)

type ZutfileManifest struct {
	Zutfile     int    `json:"zutfile"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	License     string `json:"license"`
	Runtime     struct {
		MinZut string `json:"min_zut"`
	} `json:"runtime"`
	Model struct {
		Requires   []string `json:"requires"`
		MinContext int      `json:"min_context"`
		Preferred  []string `json:"preferred"`
		MinTier    string   `json:"min_tier"`
	} `json:"model"`
	Permissions  tools.PermissionSet `json:"permissions"`
	Requirements struct {
		Bin []string `json:"bin"`
		OS  []string `json:"os"`
	} `json:"requirements"`
	Entry struct {
		Greeting      string  `json:"greeting"`
		Pre           string  `json:"pre"`
		DefaultPrompt *string `json:"default_prompt"`
	} `json:"entry"`
	ReplaceSystemPrompt bool `json:"replace_system_prompt"`
}

type zutfileLoaded struct {
	Dir      string
	Temp     bool
	Digest   string
	Manifest ZutfileManifest
}

func runZutfileCommand(rawArgs []string, version string) (bool, error) {
	if len(rawArgs) == 0 {
		return false, nil
	}
	switch rawArgs[0] {
	case "pack":
		dir := "."
		out := ""
		if len(rawArgs) > 1 {
			dir = rawArgs[1]
		}
		if len(rawArgs) > 2 {
			out = rawArgs[2]
		}
		return true, zutPack(dir, out)
	case "inspect":
		if len(rawArgs) < 2 {
			return true, fmt.Errorf("zut inspect requires a name, .zut file, directory, or GitHub URL")
		}
		return true, zutInspect(rawArgs[1])
	case "verify":
		if len(rawArgs) < 2 {
			return true, fmt.Errorf("zut verify requires a name, .zut file, directory, or GitHub URL")
		}
		zf, cleanup, err := loadZutfile(rawArgs[1])
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			return true, err
		}
		fmt.Printf("ok  digest sha256:%s\n", zf.Digest)
		return true, nil
	case "run":
		if len(rawArgs) < 2 {
			return true, fmt.Errorf("zut run requires a name, .zut file, directory, or GitHub URL")
		}
		yes, remaining := peelLeadingYes(rawArgs[1:])
		if len(remaining) < 1 {
			return true, fmt.Errorf("zut run requires a name, .zut file, directory, or GitHub URL")
		}
		ref := remaining[0]
		rest := remaining[1:]
		args, err := ParseArgs(rest)
		if err != nil {
			PrintHelp(version)
			return true, err
		}
		if yes {
			args.Yes = true
		}
		if args.Orchestrate {
			return true, fmt.Errorf("--orchestrate is not supported for packaged agents")
		}
		if err := validateOrchestrationArgs(args); err != nil {
			return true, err
		}
		return true, runLocalZutfile(ref, args, version)
	default:
		return false, nil
	}
}

func runLocalZutfile(ref string, args Args, version string) error {
	prepareRuntimeCatalog()
	zf, cleanup, err := loadZutfile(ref)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return err
	}
	if err := checkZutfileRequirements(zf, version); err != nil {
		return err
	}
	// A packaged agent has not yet had its manifest permissions attached to
	// Args. Reject orchestration at this boundary, before creating its data
	// directory or asking for permission consent.
	if args.Orchestrate {
		args.AgentName = zf.Manifest.Name
		return validateOrchestrationArgs(args)
	}
	agentData := filepath.Join(ZutHome(), "agents", safeAgentName(zf.Manifest.Name), "data")
	if err := os.MkdirAll(agentData, 0o755); err != nil {
		return err
	}
	perms := zf.Manifest.Permissions.Expand(args.CWD, agentData)
	allowed, err := consentZutfile(zf, perms, args.Yes)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}
	if err := applyZutfileModelRequirements(&args, zf.Manifest); err != nil {
		return err
	}
	agentPath := filepath.Join(zf.Dir, "AGENT.md")
	agentPrompt, err := os.ReadFile(agentPath)
	switch {
	case err == nil:
		text := strings.TrimSpace(string(agentPrompt))
		if zf.Manifest.ReplaceSystemPrompt {
			args.SystemPrompt = text
		} else {
			args.AppendSystemPrompt = append(args.AppendSystemPrompt, text)
		}
	case os.IsNotExist(err):
		// AGENT.md is optional; missing file leaves the system prompt unchanged.
	default:
		return fmt.Errorf("read AGENT.md: %w", err)
	}
	args.StartupPre = strings.TrimSpace(zf.Manifest.Entry.Pre)
	if args.Prompt == "" && zf.Manifest.Entry.DefaultPrompt != nil {
		args.Prompt = *zf.Manifest.Entry.DefaultPrompt
	}
	if dirExists(filepath.Join(zf.Dir, "skills")) {
		old := os.Getenv("ZUT_AGENT_SKILLS")
		v := filepath.Join(zf.Dir, "skills")
		if old != "" {
			v += string(os.PathListSeparator) + old
		}
		_ = os.Setenv("ZUT_AGENT_SKILLS", v)
		defer os.Setenv("ZUT_AGENT_SKILLS", old)
	}
	args.AgentName = zf.Manifest.Name
	args.AgentDataDir = agentData
	args.PermissionSet = &perms
	return runWithArgs(args, version)
}

func zutInspect(ref string) error {
	zf, cleanup, err := loadZutfile(ref)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return err
	}
	m := zf.Manifest
	fmt.Printf("name: %s\nversion: %s\ndescription: %s\ndigest: sha256:%s\n", m.Name, m.Version, m.Description, zf.Digest)
	fmt.Println("\npermissions:")
	fmt.Print(permissionSummary(m.Permissions))
	fmt.Println("\nfiles:")
	return filepath.WalkDir(zf.Dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == zf.Dir {
			return err
		}
		rel, _ := filepath.Rel(zf.Dir, path)
		if d.IsDir() {
			rel += "/"
		}
		fmt.Println("  " + filepath.ToSlash(rel))
		return nil
	})
}

func applyZutfileModelRequirements(args *Args, m ZutfileManifest) error {
	minCtx := m.Model.MinContext
	requires := map[string]bool{}
	for _, capability := range m.Model.Requires {
		requires[strings.ToLower(strings.TrimSpace(capability))] = true
	}
	for capability := range requires {
		if capability != "tools" && capability != "vision" && capability != "reasoning" {
			return fmt.Errorf("unsupported model requirement %q", capability)
		}
	}
	if m.Model.MinTier != "" {
		return fmt.Errorf("model.min_tier is not supported by this zut version")
	}
	compatible := func(model provider.Model) bool {
		if model.ContextWindow < minCtx {
			return false
		}
		if requires["reasoning"] && !model.Reasoning {
			return false
		}
		// Every model exposed by the current catalog supports text and tools.
		// Vision support is not represented in Model yet, so fail closed.
		return !requires["vision"]
	}
	if minCtx <= 0 && len(requires) == 0 {
		return nil
	}
	if args.Model != "" {
		model, err := provider.FindModel(args.Provider, args.Model)
		if err != nil {
			model, err = provider.FindModel("", args.Model)
		}
		if err != nil {
			return nil
		}
		if !compatible(model) {
			return fmt.Errorf("model %s does not satisfy the agent requirements", model.ID)
		}
		return nil
	}
	cfg, _ := LoadConfig()
	if cfg.Model != "" {
		model, err := provider.FindModel(cfg.Provider, cfg.Model)
		if err == nil && compatible(model) {
			return nil
		}
	}
	for _, id := range m.Model.Preferred {
		model, err := provider.FindModel("", id)
		if err == nil && compatible(model) {
			args.Provider = model.Provider
			args.Model = model.ID
			return nil
		}
	}
	for _, model := range provider.Active() {
		if compatible(model) {
			args.Provider = model.Provider
			args.Model = model.ID
			return nil
		}
	}
	return fmt.Errorf("no catalog model satisfies the agent requirements")
}

func prepareRuntimeCatalog() {
	LoadCachedModels()
	LoadUserModels()
	if cps := provider.CustomProviders(); len(cps) > 0 {
		var names []string
		for name := range cps {
			if !isBuiltinProvider(name) {
				names = append(names, name)
			}
		}
		auth.SetExtraAPIKeyProviders(names)
	}
	ValidateAndRepairConfig()
	RefreshModelsAsync()
}

func zutPack(dir, out string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if _, err := readZutManifest(abs); err != nil {
		return err
	}
	if err := validateZutfileDir(abs); err != nil {
		return err
	}
	if out == "" {
		m, _ := readZutManifest(abs)
		base := safeAgentName(m.Name)
		if base == "" {
			base = filepath.Base(abs)
		}
		out = base + ".zut"
	}
	if filepath.Ext(out) != ".zut" {
		out += ".zut"
	}
	outAbs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	mw := io.MultiWriter(f, h)
	enc, err := zstd.NewWriter(mw)
	if err != nil {
		return err
	}
	if err := writeCanonicalTar(abs, enc, outAbs); err != nil {
		enc.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	fmt.Printf("wrote %s\ndigest sha256:%s\n", out, hex.EncodeToString(h.Sum(nil)))
	return nil
}

func loadZutfile(ref string) (zutfileLoaded, func(), error) {
	resolved, err := resolveZutfileRef(ref)
	if err != nil {
		return zutfileLoaded{}, nil, err
	}
	ref = resolved
	if u, err := url.Parse(ref); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		return loadRemoteZutfile(u)
	}
	info, err := os.Stat(ref)
	if err != nil {
		return zutfileLoaded{}, nil, err
	}
	if info.IsDir() {
		abs, _ := filepath.Abs(ref)
		m, err := readZutManifest(abs)
		if err != nil {
			return zutfileLoaded{}, nil, err
		}
		if err := validateZutfileDir(abs); err != nil {
			return zutfileLoaded{}, nil, err
		}
		return zutfileLoaded{Dir: abs, Manifest: m, Digest: digestDirectory(abs)}, nil, nil
	}
	tmp, err := os.MkdirTemp("", "zutfile-*")
	if err != nil {
		return zutfileLoaded{}, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	digest, err := unpackZutArchive(ref, tmp)
	if err != nil {
		cleanup()
		return zutfileLoaded{}, nil, err
	}
	m, err := readZutManifest(tmp)
	if err != nil {
		cleanup()
		return zutfileLoaded{}, nil, err
	}
	if err := validateZutfileDir(tmp); err != nil {
		cleanup()
		return zutfileLoaded{}, nil, err
	}
	return zutfileLoaded{Dir: tmp, Temp: true, Digest: digest, Manifest: m}, cleanup, nil
}

func resolveZutfileRef(ref string) (string, error) {
	if u, err := url.Parse(ref); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		return ref, nil
	}
	if _, err := os.Stat(ref); err == nil {
		return ref, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if filepath.Ext(ref) == "" {
		archive := ref + ".zut"
		if _, err := os.Stat(archive); err == nil {
			return archive, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	if strings.HasSuffix(strings.ToLower(ref), ".zut") {
		return ref, nil
	}
	githubRef := strings.ReplaceAll(ref, "\\", "/")
	parts := strings.Split(githubRef, "/")
	if len(parts) == 2 && validGitHubZutfileSegment(parts[0]) && validGitHubZutfileSegment(parts[1]) {
		return "https://github.com/" + parts[0] + "/" + parts[1], nil
	}
	if len(parts) >= 3 {
		for _, part := range parts {
			if !validGitHubZutfileSegment(part) {
				return ref, nil
			}
		}
		return "https://github.com/" + strings.Join(parts, "/"), nil
	}
	return ref, nil
}

func validGitHubZutfileSegment(s string) bool {
	return s != "" && s == strings.ToLower(s) && safeAgentName(s) == s
}

var zutfileHTTPClient = &http.Client{Timeout: 60 * time.Second}

var githubArchiveURL = func(owner, repo, ref string) string {
	return fmt.Sprintf("https://github.com/%s/%s/archive/%s.tar.gz", owner, repo, url.PathEscape(ref))
}

func loadRemoteZutfile(u *url.URL) (zutfileLoaded, func(), error) {
	if !strings.EqualFold(u.Hostname(), "github.com") {
		return zutfileLoaded{}, nil, fmt.Errorf("unsupported zutfile URL host %q; only github.com agent directories are supported", u.Hostname())
	}
	owner, repo, ref, subdir, err := parseGitHubAgentURL(u)
	if err != nil {
		return zutfileLoaded{}, nil, err
	}
	tmp, err := os.MkdirTemp("", "zutfile-github-*")
	if err != nil {
		return zutfileLoaded{}, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	if err := downloadGitHubArchive(githubArchiveURL(owner, repo, ref), tmp); err != nil {
		cleanup()
		return zutfileLoaded{}, nil, err
	}
	root, err := singleExtractedRoot(tmp)
	if err != nil {
		cleanup()
		return zutfileLoaded{}, nil, err
	}
	dir := filepath.Join(root, filepath.FromSlash(subdir))
	if subdir == "" {
		dir = root
	}
	if !pathWithin(root, dir) {
		cleanup()
		return zutfileLoaded{}, nil, fmt.Errorf("unsafe GitHub agent path %q", subdir)
	}
	m, err := readZutManifest(dir)
	if err != nil {
		cleanup()
		return zutfileLoaded{}, nil, fmt.Errorf("GitHub agent %s/%s/%s: %w", owner, repo, subdir, err)
	}
	if err := validateZutfileDir(dir); err != nil {
		cleanup()
		return zutfileLoaded{}, nil, err
	}
	return zutfileLoaded{Dir: dir, Temp: true, Digest: digestDirectory(dir), Manifest: m}, cleanup, nil
}

func parseGitHubAgentURL(u *url.URL) (owner, repo, ref, subdir string, err error) {
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	for i := range parts {
		parts[i], err = url.PathUnescape(parts[i])
		if err != nil {
			return "", "", "", "", fmt.Errorf("invalid GitHub URL path: %w", err)
		}
	}
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", "", fmt.Errorf("GitHub agent URL must include an owner and repository")
	}
	owner, repo, ref = parts[0], strings.TrimSuffix(parts[1], ".git"), "HEAD"
	if len(parts) >= 4 && parts[2] == "tree" {
		ref = parts[3]
		subdir = strings.Join(parts[4:], "/")
	} else {
		subdir = strings.Join(parts[2:], "/")
	}
	if repo == "" || ref == "" {
		return "", "", "", "", fmt.Errorf("invalid GitHub agent URL")
	}
	clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(subdir))), "./")
	if clean == "." {
		clean = ""
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", "", "", fmt.Errorf("unsafe GitHub agent path %q", subdir)
	}
	return owner, repo, ref, clean, nil
}

func downloadGitHubArchive(archiveURL, dst string) error {
	req, err := http.NewRequest(http.MethodGet, archiveURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "zut")
	resp, err := zutfileHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("download GitHub repository: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download GitHub repository: HTTP %d", resp.StatusCode)
	}
	limited := &io.LimitedReader{R: resp.Body, N: maxZutfileCompressedSize + 1}
	gr, err := gzip.NewReader(limited)
	if err != nil {
		return fmt.Errorf("read GitHub archive: %w", err)
	}
	defer gr.Close()
	if err := untar(gr, dst); err != nil {
		return fmt.Errorf("extract GitHub archive: %w", err)
	}
	if limited.N <= 0 {
		return fmt.Errorf("GitHub archive exceeds %d MiB compressed size limit", maxZutfileCompressedSize>>20)
	}
	return nil
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func singleExtractedRoot(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return "", fmt.Errorf("GitHub archive has an unexpected layout")
	}
	return filepath.Join(dir, entries[0].Name()), nil
}

func readZutManifest(dir string) (ZutfileManifest, error) {
	var m ZutfileManifest
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return m, fmt.Errorf("manifest.json is required: %w", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("manifest.json: %w", err)
	}
	if m.Zutfile != 1 {
		return m, fmt.Errorf("unsupported zutfile version %d", m.Zutfile)
	}
	name := strings.TrimSpace(m.Name)
	if name == "" {
		return m, fmt.Errorf("manifest name is required")
	}
	if name != strings.ToLower(name) || safeAgentName(name) != name {
		return m, fmt.Errorf("manifest name must contain only lowercase letters, digits, dots, hyphens, or underscores")
	}
	if len(m.Permissions.Net.Allow) > 0 {
		return m, fmt.Errorf("permissions.net is not supported by the local runtime yet")
	}
	if len(m.Permissions.Env.Read) > 0 {
		return m, fmt.Errorf("permissions.env is not supported by the local runtime yet")
	}
	mode := strings.ToLower(strings.TrimSpace(m.Permissions.Bash.Mode))
	if mode != "" && mode != "none" && mode != "ask" && mode != "allowlist" {
		return m, fmt.Errorf("unsupported bash permission mode %q", m.Permissions.Bash.Mode)
	}
	if mode == "allowlist" && len(m.Permissions.Bash.Allow) == 0 {
		return m, fmt.Errorf("bash allowlist mode requires at least one command")
	}
	return m, nil
}

func validateZutfileDir(dir string) error {
	agentPath := filepath.Join(dir, "AGENT.md")
	st, err := os.Stat(agentPath)
	switch {
	case err == nil:
		if !st.Mode().IsRegular() {
			return fmt.Errorf("AGENT.md must be a regular file")
		}
	case os.IsNotExist(err):
		// AGENT.md is optional.
	default:
		return fmt.Errorf("stat AGENT.md: %w", err)
	}
	if exts := bundledExtensionDirs(filepath.Join(dir, "extensions")); len(exts) > 0 {
		return fmt.Errorf("bundled executable extensions are not supported by the local runtime: they cannot yet be confined to manifest permissions")
	}
	return nil
}

const (
	maxZutfileCompressedSize = 100 << 20
	maxZutfileEntrySize      = 64 << 20
	maxZutfileExpandedSize   = 256 << 20
)

func unpackZutArchive(path, dst string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > maxZutfileCompressedSize {
		return "", fmt.Errorf("zutfile exceeds %d MiB compressed size limit", maxZutfileCompressedSize>>20)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(b)
	r, err := zstd.NewReader(bytes.NewReader(b), zstd.WithDecoderMaxMemory(maxZutfileExpandedSize))
	if err != nil {
		// Development fallback for older experiments.
		gr, gerr := gzip.NewReader(bytes.NewReader(b))
		if gerr != nil {
			return "", err
		}
		defer gr.Close()
		return hex.EncodeToString(digest[:]), untar(gr, dst)
	}
	defer r.Close()
	return hex.EncodeToString(digest[:]), untar(r, dst)
}

func untar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	var expanded int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("unsafe path in archive: %s", h.Name)
		}
		path := filepath.Join(dst, name)
		if h.Size < 0 || h.Size > maxZutfileEntrySize || expanded+h.Size > maxZutfileExpandedSize {
			return fmt.Errorf("archive content exceeds extraction size limit: %s", h.Name)
		}
		expanded += h.Size
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o777)
			if err != nil {
				return err
			}
			_, cerr := io.Copy(f, tr)
			if err := f.Close(); err != nil && cerr == nil {
				cerr = err
			}
			if cerr != nil {
				return cerr
			}
		}
	}
}

func writeCanonicalTar(root string, w io.Writer, exclude ...string) error {
	excluded := map[string]bool{}
	for _, p := range exclude {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err == nil {
			excluded[abs] = true
		}
	}
	var files []string
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		absPath, _ := filepath.Abs(path)
		if excluded[absPath] {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinks are not supported in zutfiles: %s", path)
		}
		files = append(files, path)
		return nil
	}); err != nil {
		return err
	}
	sort.Strings(files)
	tw := tar.NewWriter(w)
	defer tw.Close()
	fixed := time.Unix(0, 0)
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name = rel
		h.ModTime = fixed
		h.AccessTime = fixed
		h.ChangeTime = fixed
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if info.IsDir() && !strings.HasSuffix(h.Name, "/") {
			h.Name += "/"
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, cerr := io.Copy(tw, f)
			_ = f.Close()
			if cerr != nil {
				return cerr
			}
		}
	}
	return nil
}

func checkZutfileRequirements(zf zutfileLoaded, version string) error {
	if min := strings.TrimSpace(zf.Manifest.Runtime.MinZut); min != "" {
		if isDevVersion(version) {
			return fmt.Errorf("agent requires zut %s or newer; unversioned development builds cannot satisfy min_zut", min)
		}
		if versionLess(version, min) {
			return fmt.Errorf("agent requires zut %s or newer; running %s", min, versionOnly(version))
		}
	}
	if len(zf.Manifest.Requirements.OS) > 0 {
		ok := false
		for _, osName := range zf.Manifest.Requirements.OS {
			if osName == runtime.GOOS {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("agent does not support %s", runtime.GOOS)
		}
	}
	if len(zf.Manifest.Requirements.Bin) > 0 {
		for _, b := range zf.Manifest.Requirements.Bin {
			if _, err := execLookPath(b); err != nil {
				return fmt.Errorf("agent requires binary %q", b)
			}
		}
	}
	return nil
}

var execLookPath = exec.LookPath

func peelLeadingYes(args []string) (yes bool, rest []string) {
	rest = args
	for len(rest) > 0 {
		switch rest[0] {
		case "-y", "--yes":
			yes = true
			rest = rest[1:]
		default:
			return yes, rest
		}
	}
	return yes, rest
}

func consentZutfile(zf zutfileLoaded, perms tools.PermissionSet, autoYes bool) (bool, error) {
	if autoYes || os.Getenv("ZUT_AGENT_CONSENT") == "1" {
		return acceptZutfileConsent(zf, perms)
	}
	// "ask" deliberately requires approval on every launch. Other consent
	// is durable only for this exact artifact digest, so any package change
	// causes a fresh prompt.
	consentPath := filepath.Join(ZutHome(), "agents", safeAgentName(zf.Manifest.Name), "consents", zf.Digest+".json")
	if strings.ToLower(strings.TrimSpace(perms.Bash.Mode)) != "ask" {
		if _, err := os.Stat(consentPath); err == nil {
			return true, nil
		}
	}
	interactive := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	th := tui.Dark
	if interactive {
		cfg, _ := LoadConfig()
		th, _, _ = tui.DetectThemeWithCustom(ZutHome(), cfg.Theme, 80*time.Millisecond)
	}
	fmt.Print(formatZutfileConsent(zf, perms, th, interactive))
	if !interactive {
		return false, fmt.Errorf("refusing to run without interactive consent; pass -y/--yes or set ZUT_AGENT_CONSENT=1 to allow")
	}
	allowed, err := readZutfileConsent(os.Stdin, os.Stdout, th)
	if err != nil || !allowed {
		return allowed, err
	}
	return acceptZutfileConsent(zf, perms)
}

// acceptZutfileConsent records durable consent when the bash mode is not
// "ask". Ask mode never caches receipts so every launch re-prompts unless
// -y / ZUT_AGENT_CONSENT is used for that run.
func acceptZutfileConsent(zf zutfileLoaded, perms tools.PermissionSet) (bool, error) {
	if strings.ToLower(strings.TrimSpace(perms.Bash.Mode)) == "ask" {
		return true, nil
	}
	consentPath := filepath.Join(ZutHome(), "agents", safeAgentName(zf.Manifest.Name), "consents", zf.Digest+".json")
	if err := os.MkdirAll(filepath.Dir(consentPath), 0o700); err != nil {
		return false, fmt.Errorf("save agent consent: %w", err)
	}
	receipt := map[string]string{"digest": zf.Digest, "name": zf.Manifest.Name, "version": zf.Manifest.Version}
	data, _ := json.MarshalIndent(receipt, "", "  ")
	if err := os.WriteFile(consentPath, append(data, '\n'), 0o600); err != nil {
		return false, fmt.Errorf("save agent consent: %w", err)
	}
	return true, nil
}

var errZutfileConsentInterrupted = errors.New("interrupted")

func formatZutfileConsent(zf zutfileLoaded, perms tools.PermissionSet, th tui.Theme, color bool) string {
	style := func(c tui.TerminalColor, text string) string {
		if !color {
			return text
		}
		return th.FGColor(c, text)
	}

	var sb strings.Builder
	sb.WriteString(style(th.Assistant, "Agent"))
	sb.WriteByte(' ')
	sb.WriteString(style(th.Accent, zf.Manifest.Name+"@"+zf.Manifest.Version))
	sb.WriteString(style(th.FG, " wants to run."))
	sb.WriteString("\n\n")
	sb.WriteString(permissionSummaryStyled(perms, th, color))
	if color {
		sb.WriteByte('\n')
		sb.WriteString(style(th.Assistant, "Allow?"))
		sb.WriteByte(' ')
		sb.WriteString(style(th.Muted, "[y/n]"))
		sb.WriteByte(' ')
	}
	return sb.String()
}

// readZutfileConsent accepts one y/n answer followed by Enter without echoing
// unrelated input. Raw mode also turns Ctrl+C into a byte so the terminal can
// be restored before the command aborts.
func readZutfileConsent(in *os.File, out io.Writer, th tui.Theme) (bool, error) {
	fd := int(in.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return false, fmt.Errorf("read agent consent: %w", err)
	}
	defer func() { _ = term.Restore(fd, state) }()

	answer, err := readZutfileConsentKey(in, func(answer byte) {
		if answer == 0 {
			fmt.Fprint(out, "\b \b")
			return
		}
		fmt.Fprint(out, th.FGColor(th.FG, string(answer)))
	})
	switch {
	case errors.Is(err, errZutfileConsentInterrupted):
		fmt.Fprint(out, "^C\r\n")
		return false, err
	case err != nil:
		fmt.Fprint(out, "\r\n")
		return false, fmt.Errorf("read agent consent: %w", err)
	default:
		fmt.Fprint(out, "\r\n")
		return answer == 'y', nil
	}
}

func readZutfileConsentKey(r io.Reader, echo func(byte)) (byte, error) {
	var answer byte
	var b [1]byte
	for {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, err
		}
		switch b[0] {
		case 'y', 'Y', 'n', 'N':
			if answer == 0 {
				answer = b[0] | 0x20
				if echo != nil {
					echo(answer)
				}
			}
		case '\r', '\n':
			if answer != 0 {
				return answer, nil
			}
		case '\b', 0x7f:
			if answer != 0 {
				answer = 0
				if echo != nil {
					echo(0)
				}
			}
		case 3: // Ctrl+C in raw mode.
			return 0, errZutfileConsentInterrupted
		}
	}
}

func permissionSummary(p tools.PermissionSet) string {
	return permissionSummaryStyled(p, tui.Theme{}, false)
}

func permissionSummaryStyled(p tools.PermissionSet, th tui.Theme, color bool) string {
	style := func(c tui.TerminalColor, text string) string {
		if !color {
			return text
		}
		return th.FGColor(c, text)
	}
	permissionValue := func(value string) string { return style(th.FG, value) }

	var sb strings.Builder
	sb.WriteString(style(th.Muted, "  fs read: "))
	if len(p.FS.Read) > 0 {
		sb.WriteString(permissionValue(strings.Join(p.FS.Read, ", ")))
	} else {
		sb.WriteString(style(th.Muted, "none"))
	}
	sb.WriteByte('\n')
	sb.WriteString(style(th.Muted, "  fs write: "))
	if len(p.FS.Write) > 0 {
		sb.WriteString(permissionValue(strings.Join(p.FS.Write, ", ")))
	} else {
		sb.WriteString(style(th.Muted, "none"))
	}
	sb.WriteByte('\n')
	mode := p.Bash.Mode
	if mode == "" {
		mode = "none"
	}
	sb.WriteString(style(th.Muted, "  bash: "))
	modeColor := th.FG
	if mode == "ask" {
		modeColor = th.Warning
	}
	sb.WriteString(style(modeColor, mode))
	if len(p.Bash.Allow) > 0 {
		sb.WriteString(style(th.Muted, " ("))
		sb.WriteString(permissionValue(strings.Join(p.Bash.Allow, ", ")))
		sb.WriteString(style(th.Muted, ")"))
	}
	sb.WriteByte('\n')
	if len(p.Net.Allow) > 0 {
		sb.WriteString(style(th.Muted, "  net: "))
		sb.WriteString(permissionValue(strings.Join(p.Net.Allow, ", ")))
		sb.WriteString(style(th.Warning, " (declared, not enforced in this build)\n"))
	}
	if len(p.Env.Read) > 0 {
		sb.WriteString(style(th.Muted, "  env read: "))
		sb.WriteString(permissionValue(strings.Join(p.Env.Read, ", ")))
		sb.WriteString(style(th.Warning, " (declared, not enforced in this build)\n"))
	}
	return sb.String()
}

func bundledExtensionDirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && fileExists(filepath.Join(root, e.Name(), "extension.json")) {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	return out
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
func dirExists(p string) bool  { st, err := os.Stat(p); return err == nil && st.IsDir() }

func safeAgentName(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, s)
	return strings.Trim(s, "-._")
}

func digestDirectory(dir string) string {
	h := sha256.New()
	_ = writeCanonicalTar(dir, h)
	return hex.EncodeToString(h.Sum(nil))
}
