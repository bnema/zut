package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/extensions"
	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/ignore"
)

// runExtCommand dispatches `zut ext ...` subcommands. Returns
// (handled=true, err) if rawArgs starts with "ext"; otherwise
// (handled=false, nil) so the main router falls through to the
// regular flag parser.
func runExtCommand(ctx context.Context, rawArgs []string, version string) (handled bool, err error) {
	if len(rawArgs) == 0 || rawArgs[0] != "ext" {
		return false, nil
	}
	if len(rawArgs) == 1 {
		printExtHelp()
		return true, nil
	}
	switch rawArgs[1] {
	case "list":
		return true, extList()
	case "doctor":
		return true, extDoctor(version)
	case "logs":
		return true, extLogs(rawArgs[2:])
	case "enable":
		return true, extToggle(rawArgs[2:], true)
	case "disable":
		return true, extToggle(rawArgs[2:], false)
	case "remove", "rm":
		return true, extRemove(rawArgs[2:])
	case "install":
		return true, extInstallContext(ctx, rawArgs[2:])
	case "help", "-h", "--help":
		printExtHelp()
		return true, nil
	default:
		printExtHelp()
		return true, fmt.Errorf("unknown ext subcommand: %s", rawArgs[1])
	}
}

func printExtHelp() {
	fmt.Fprintln(os.Stderr, `zut ext — manage extensions

usage:
  zut ext list                    list installed extensions and their state
  zut ext doctor                  diagnose installed extensions
  zut ext logs <name> [-f]        cat / tail an extension's stderr log
  zut ext enable <name>           re-enable a disabled extension
  zut ext disable <name>          disable without removing
  zut ext remove <name>                    delete an extension directory
  zut ext install [--build=go] <path|git-url>
                                         copy / clone and validate an extension
                                         --build=go explicitly builds a local Go extension

extensions live under:
  $ZUT_HOME/extensions/<name>/extension.json   (global)
  ./.zut/extensions/<name>/extension.json      (project-local)`)
}

// extList walks both the global and project-local extension dirs and
// prints a one-row-per-extension table.
func extList() error {
	type row struct {
		Scope    string
		Name     string
		Version  string
		Enabled  string
		Language string
		Dir      string
	}
	var rows []row
	for scope, dir := range extensionDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			extDir := filepath.Join(dir, e.Name())
			mfPath := filepath.Join(extDir, "extension.json")
			raw, err := os.ReadFile(mfPath)
			if err != nil {
				continue
			}
			var m struct {
				Name     string `json:"name"`
				Version  string `json:"version"`
				Language string `json:"language"`
				Enabled  *bool  `json:"enabled"`
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				continue
			}
			enabled := "yes"
			if m.Enabled != nil && !*m.Enabled {
				enabled = "no"
			}
			rows = append(rows, row{
				Scope: scope, Name: m.Name, Version: m.Version,
				Enabled: enabled, Language: m.Language, Dir: extDir,
			})
		}
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "no extensions installed")
		fmt.Fprintln(os.Stderr, "see docs/extensions.md to write your own, or `zut ext install <path|url>`")
		return nil
	}
	fmt.Printf("%-12s  %-20s  %-10s  %-8s  %-10s  %s\n", "scope", "name", "version", "enabled", "language", "dir")
	for _, r := range rows {
		fmt.Printf("%-12s  %-20s  %-10s  %-8s  %-10s  %s\n",
			r.Scope, r.Name, dashIfEmpty(r.Version),
			r.Enabled, dashIfEmpty(r.Language), r.Dir)
	}
	return nil
}

type extDoctorHooks struct{}

func (extDoctorHooks) Notify(string, string, string)                        {}
func (extDoctorHooks) Alert(string, extproto.AlertRequest)                  {}
func (extDoctorHooks) Submit(string)                                        {}
func (extDoctorHooks) SubmitSlash(string)                                   {}
func (extDoctorHooks) Insert(string)                                        {}
func (extDoctorHooks) Display(string, string)                               {}
func (extDoctorHooks) ClearNotes(string)                                    {}
func (extDoctorHooks) OpenPanel(string, extproto.PanelSpec)                 {}
func (extDoctorHooks) UpdatePanel(string, string, string, []string, string) {}
func (extDoctorHooks) ClosePanel(string, string)                            {}
func (extDoctorHooks) SetStatus(string, string, string, string)             {}
func (extDoctorHooks) SetWidget(string, string, string, string, []string)   {}
func (extDoctorHooks) ClearWidget(string, string)                           {}

type extDoctorStaticRow struct {
	Scope    string
	Name     string
	Version  string
	Enabled  bool
	Dir      string
	Exec     string
	Theme    bool
	Manifest string
	Error    string
	Shadowed bool
}

// extDoctor diagnoses extension discovery and registration without changing
// normal fail-soft extension behavior.
func extDoctor(version string) error {
	rows := scanExtDoctorStatic()
	if len(rows) == 0 {
		fmt.Fprintln(os.Stdout, "no extensions installed")
		fmt.Fprintln(os.Stdout, "see docs/extensions.md to write your own, or `zut ext install <path|url>`")
		return nil
	}

	cwd, _ := os.Getwd()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mgr := extensions.New(ZutHome(), cwd, version, "", "", extDoctorHooks{})
	errs := mgr.Discover(ctx)
	mgr.WaitForReady(3 * time.Second)
	diags := mgr.Diagnostics()
	mgr.Stop(500 * time.Millisecond)

	diagByDir := map[string]extensions.ExtensionDiagnostic{}
	for _, d := range diags {
		diagByDir[d.Dir] = d
	}

	fmt.Fprintln(os.Stdout, "zut extension doctor")
	fmt.Fprintln(os.Stdout)
	for _, row := range rows {
		printExtDoctorRow(os.Stdout, row, diagByDir[row.Dir])
	}
	if len(errs) > 0 {
		fmt.Fprintln(os.Stdout, "load errors:")
		for _, err := range errs {
			// Multiline diagnostics keep their line breaks; indent
			// continuations so they read as one entry.
			indented := strings.ReplaceAll(err.Error(), "\n", "\n    ")
			fmt.Fprintf(os.Stdout, "  ! %s\n", indented)
		}
	}
	return nil
}

func scanExtDoctorStatic() []extDoctorStaticRow {
	type scanDir struct {
		scope string
		dir   string
	}
	var dirs []scanDir
	if cwd, err := os.Getwd(); err == nil {
		dirs = append(dirs, scanDir{scope: "project", dir: filepath.Join(cwd, ".zut", "extensions")})
	}
	if h := ZutHome(); h != "" {
		dirs = append(dirs, scanDir{scope: "global", dir: filepath.Join(h, "extensions")})
	}

	seen := map[string]bool{}
	var rows []extDoctorStaticRow
	for _, sd := range dirs {
		entries, err := os.ReadDir(sd.dir)
		if err != nil {
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			extDir := filepath.Join(sd.dir, e.Name())
			row := extDoctorStaticRow{
				Scope:    sd.scope,
				Name:     e.Name(),
				Enabled:  true,
				Dir:      extDir,
				Manifest: filepath.Join(extDir, "extension.json"),
				Theme:    extensions.HasExtensionTheme(extDir),
			}
			if seen[e.Name()] {
				row.Shadowed = true
			} else {
				seen[e.Name()] = true
			}
			raw, err := os.ReadFile(row.Manifest)
			if err != nil {
				row.Error = "missing extension.json"
				rows = append(rows, row)
				continue
			}
			var m struct {
				Name    string `json:"name"`
				Version string `json:"version"`
				Exec    string `json:"exec"`
				Enabled *bool  `json:"enabled"`
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				row.Error = "parse manifest: " + err.Error()
				rows = append(rows, row)
				continue
			}
			if m.Name != "" {
				row.Name = m.Name
			} else {
				row.Error = "manifest: name is required"
			}
			row.Version = m.Version
			row.Exec = m.Exec
			if m.Enabled != nil {
				row.Enabled = *m.Enabled
			}
			if row.Exec == "" && !row.Theme && row.Error == "" {
				row.Error = "manifest: exec is required"
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func printExtDoctorRow(w io.Writer, row extDoctorStaticRow, diag extensions.ExtensionDiagnostic) {
	status := "ok"
	switch {
	case row.Error != "":
		status = "error"
	case row.Shadowed:
		status = "shadowed"
	case !row.Enabled:
		status = "disabled"
	case row.Exec == "" && row.Theme:
		status = "theme-only"
	case diag.Name == "":
		status = "not loaded"
	case diag.ReadyTimedOut:
		status = "ready-timeout"
	case diag.AutoReady:
		status = "auto-ready"
	case !diag.Ready:
		status = "not ready"
	case len(diag.Messages) > 0:
		status = "warning"
	}
	fmt.Fprintf(w, "%s [%s] %s (%s)\n", row.Name, row.Scope, status, row.Dir)
	if row.Version != "" {
		fmt.Fprintf(w, "  version: %s\n", row.Version)
	}
	if row.Error != "" {
		if row.Exec != "" {
			fmt.Fprintf(w, "  log: %s\n", extDoctorLogPath(row.Name))
		}
		fmt.Fprintf(w, "  error: %s\n", row.Error)
		return
	}
	if row.Shadowed {
		fmt.Fprintln(w, "  note: skipped because a higher-priority extension directory with this name wins")
		return
	}
	if !row.Enabled {
		fmt.Fprintln(w, "  note: disabled in extension.json")
		return
	}
	if row.Exec == "" && row.Theme {
		fmt.Fprintln(w, "  type: theme-only")
		return
	}
	if row.Exec != "" {
		fmt.Fprintf(w, "  exec: %s\n", row.Exec)
	}
	logPath := diag.LogPath
	if logPath == "" && row.Exec != "" {
		logPath = extDoctorLogPath(row.Name)
	}
	if logPath != "" {
		fmt.Fprintf(w, "  log: %s\n", logPath)
	}
	if len(diag.Commands) > 0 {
		fmt.Fprintln(w, "  commands:")
		for _, c := range diag.Commands {
			state := "active"
			if !c.Active {
				state = "shadowed"
			}
			fmt.Fprintf(w, "    /%s (%s)\n", c.Name, state)
		}
	}
	if len(diag.Tools) > 0 {
		fmt.Fprintln(w, "  tools:")
		for _, t := range diag.Tools {
			state := "active"
			if !t.Active {
				state = "shadowed"
			}
			fmt.Fprintf(w, "    %s (%s)\n", t.Name, state)
		}
	}
	for _, msg := range diag.Messages {
		fmt.Fprintf(w, "  warning: %s\n", msg)
	}
}

func extDoctorLogPath(name string) string {
	if name == "" || ZutHome() == "" {
		return ""
	}
	return filepath.Join(ZutHome(), "logs", "ext-"+name+".log")
}

// extLogs locates the named extension's log file and either cats or
// tails it (-f).
func extLogs(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: zut ext logs <name> [-f]")
	}
	name := args[0]
	follow := false
	for _, a := range args[1:] {
		if a == "-f" || a == "--follow" {
			follow = true
		}
	}
	logPath := filepath.Join(ZutHome(), "logs", "ext-"+name+".log")
	if _, err := os.Stat(logPath); err != nil {
		return fmt.Errorf("no log for %q at %s", name, logPath)
	}
	if !follow {
		f, err := os.Open(logPath)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(os.Stdout, f)
		return err
	}
	cmd := exec.Command("tail", "-F", logPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// extToggle flips the enabled flag in an extension's manifest.
func extToggle(args []string, enabled bool) error {
	if len(args) == 0 {
		verb := "enable"
		if !enabled {
			verb = "disable"
		}
		return fmt.Errorf("usage: zut ext %s <name>", verb)
	}
	name := args[0]
	dir, err := findExtensionDir(name)
	if err != nil {
		return err
	}
	mfPath := filepath.Join(dir, "extension.json")
	raw, err := os.ReadFile(mfPath)
	if err != nil {
		return err
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	generic["enabled"] = enabled
	out, err := json.MarshalIndent(generic, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(mfPath, append(out, '\n'), 0o644); err != nil {
		return err
	}
	state := "enabled"
	if !enabled {
		state = "disabled"
	}
	fmt.Fprintf(os.Stderr, "%s %s\n", state, name)
	return nil
}

// extRemove deletes an extension's directory after a confirmation
// prompt (skip with --yes).
func extRemove(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: zut ext remove <name> [--yes]")
	}
	name := args[0]
	yes := false
	for _, a := range args[1:] {
		if a == "--yes" || a == "-y" {
			yes = true
		}
	}
	dir, err := findExtensionDir(name)
	if err != nil {
		return err
	}
	if !yes {
		fmt.Fprintf(os.Stderr, "remove %s ? [y/N] ", dir)
		var resp string
		_, _ = fmt.Scanln(&resp)
		if !strings.EqualFold(strings.TrimSpace(resp), "y") {
			fmt.Fprintln(os.Stderr, "aborted")
			return nil
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "removed %s\n", dir)
	return nil
}

// extInstall copies a local directory or shallow-clones a git URL
// into $ZUT_HOME/extensions/. It stages the install and validates the
// manifest plus any extension-local executable before reporting success.
// Builds are never inferred or run implicitly; --build=go is an explicit
// opt-in for local Go extensions.
func extInstall(args []string) error {
	return extInstallContext(context.Background(), args)
}

func extInstallContext(ctx context.Context, args []string) error {
	opts, err := parseExtInstallArgs(args)
	if err != nil {
		return err
	}
	src := opts.source
	dest := filepath.Join(ZutHome(), "extensions")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	if strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "git@") || strings.HasSuffix(src, ".git") {
		if opts.builder != "" {
			return errors.New("--build=go is currently supported only for local extension paths; clone the extension locally and build it there")
		}
		return installGitExtension(ctx, src, dest)
	}

	// Local path: must be a directory containing extension.json.
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", src)
	}
	// Resolve to an absolute, cleaned path before reading the manifest.
	// This also makes sources like "." safe to install.
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	manifest, err := readExtensionManifest(absSrc)
	if err != nil {
		return fmt.Errorf("source extension: %w", err)
	}
	out := filepath.Join(dest, manifest.Name)
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("destination %s already exists; remove it first", out)
	}

	stageRoot, err := os.MkdirTemp(dest, ".zut-extension-install-")
	if err != nil {
		return fmt.Errorf("create install staging directory: %w", err)
	}
	defer os.RemoveAll(stageRoot)
	staged := filepath.Join(stageRoot, "extension")
	if err := copyDirWithRequired(absSrc, staged, manifestRuntimeFiles(absSrc, manifest)); err != nil {
		return fmt.Errorf("copy extension: %w", err)
	}
	if opts.builder != "" {
		if err := buildLocalExtension(ctx, absSrc, staged, manifest); err != nil {
			return err
		}
	}
	if err := validateExtensionExecutable(staged, manifest); err != nil {
		return err
	}
	if err := os.Rename(staged, out); err != nil {
		return fmt.Errorf("finalize install: %w", err)
	}
	fmt.Fprintf(os.Stderr, "installed %s\n", out)
	return nil
}

type extInstallOptions struct {
	source  string
	builder string
}

func parseExtInstallArgs(args []string) (extInstallOptions, error) {
	if len(args) == 0 {
		return extInstallOptions{}, errors.New("usage: zut ext install [--build=go] <path|git-url>")
	}

	var opts extInstallOptions
	builderSet := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--build":
			if i+1 >= len(args) {
				return extInstallOptions{}, errors.New("--build requires a builder (currently: go)")
			}
			i++
			if builderSet {
				return extInstallOptions{}, errors.New("extension builder specified more than once")
			}
			builderSet = true
			opts.builder = strings.ToLower(strings.TrimSpace(args[i]))
			if opts.builder == "" {
				return extInstallOptions{}, errors.New("--build requires a builder (currently: go)")
			}
		case strings.HasPrefix(arg, "--build="):
			if builderSet {
				return extInstallOptions{}, errors.New("extension builder specified more than once")
			}
			builderSet = true
			opts.builder = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(arg, "--build=")))
			if opts.builder == "" {
				return extInstallOptions{}, errors.New("--build requires a builder (currently: go)")
			}
		case strings.HasPrefix(arg, "-"):
			return extInstallOptions{}, fmt.Errorf("unknown ext install option %q", arg)
		case opts.source != "":
			return extInstallOptions{}, errors.New("usage: zut ext install [--build=go] <path|git-url>")
		default:
			opts.source = arg
		}
	}
	if opts.source == "" {
		return extInstallOptions{}, errors.New("usage: zut ext install [--build=go] <path|git-url>")
	}
	if opts.builder != "" && opts.builder != "go" {
		return extInstallOptions{}, fmt.Errorf("unsupported extension builder %q (currently supported: go)", opts.builder)
	}
	return opts, nil
}

func buildLocalExtension(ctx context.Context, sourceDir, stagedDir string, manifest extensions.Manifest) error {
	if manifest.Exec == "" {
		return errors.New("cannot build a theme-only extension; it does not declare an executable")
	}
	execRel, err := extensionRelativeExecPath(sourceDir, manifest.Exec)
	if err != nil {
		return fmt.Errorf("%w: --build=go requires an extension-relative executable path", err)
	}
	stagedExec := filepath.Join(stagedDir, execRel)
	if err := os.MkdirAll(filepath.Dir(stagedExec), 0o755); err != nil {
		return fmt.Errorf("prepare Go build output: %w", err)
	}

	fmt.Fprintf(os.Stderr, "building %s with go\n", manifest.Name)
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", stagedExec, ".")
	cmd.Dir = sourceDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build extension with go: %w", err)
	}
	return nil
}

func extensionRelativeExecPath(dir, execName string) (string, error) {
	normalized := filepath.FromSlash(execName)
	if filepath.IsAbs(normalized) {
		return "", fmt.Errorf("extension executable %q is absolute", execName)
	}
	resolved, local := extensions.ResolveExecPath(dir, execName)
	if !local {
		return "", fmt.Errorf("extension executable %q is a PATH command", execName)
	}
	rel, err := filepath.Rel(dir, resolved)
	if err != nil {
		return "", fmt.Errorf("resolve extension executable %q: %w", execName, err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("extension executable %q escapes the extension directory", execName)
	}
	return rel, nil
}

func installGitExtension(ctx context.Context, src, dest string) error {
	stageRoot, err := os.MkdirTemp(dest, ".zut-extension-clone-")
	if err != nil {
		return fmt.Errorf("create install staging directory: %w", err)
	}
	defer os.RemoveAll(stageRoot)

	cloneDir := filepath.Join(stageRoot, "extension")
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", src, cloneDir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}
	manifest, err := readExtensionManifest(cloneDir)
	if err != nil {
		return fmt.Errorf("cloned extension: %w", err)
	}
	out := filepath.Join(dest, manifest.Name)
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("destination %s already exists; remove it first", out)
	}
	if err := validateExtensionExecutable(cloneDir, manifest); err != nil {
		return err
	}
	if err := os.Rename(cloneDir, out); err != nil {
		return fmt.Errorf("finalize install: %w", err)
	}
	fmt.Fprintf(os.Stderr, "installed %s\n", out)
	return nil
}

func readExtensionManifest(dir string) (extensions.Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "extension.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return extensions.Manifest{}, errors.New("source lacks extension.json")
		}
		return extensions.Manifest{}, fmt.Errorf("read extension.json: %w", err)
	}
	var manifest extensions.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return extensions.Manifest{}, fmt.Errorf("parse extension.json: %w", err)
	}
	if strings.TrimSpace(manifest.Name) == "" {
		return extensions.Manifest{}, errors.New("manifest: name is required")
	}
	if manifest.Name != strings.TrimSpace(manifest.Name) {
		return extensions.Manifest{}, fmt.Errorf("manifest: extension name %q has leading or trailing whitespace", manifest.Name)
	}
	if strings.HasPrefix(manifest.Name, ".") || strings.ContainsAny(manifest.Name, `/\\`) {
		return extensions.Manifest{}, fmt.Errorf("manifest: invalid extension name %q", manifest.Name)
	}
	if manifest.Exec == "" && !extensions.HasExtensionTheme(dir) {
		return extensions.Manifest{}, errors.New("manifest: exec is required")
	}
	if manifest.Exec != "" {
		resolved, local := extensions.ResolveExecPath(dir, manifest.Exec)
		if local && !filepath.IsAbs(filepath.FromSlash(manifest.Exec)) {
			rel, err := filepath.Rel(dir, resolved)
			if err != nil {
				return extensions.Manifest{}, fmt.Errorf("manifest: resolve exec %q: %w", manifest.Exec, err)
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return extensions.Manifest{}, fmt.Errorf("manifest: exec %q escapes the extension directory", manifest.Exec)
			}
		}
	}
	return manifest, nil
}

func manifestRuntimeFiles(dir string, manifest extensions.Manifest) map[string]bool {
	required := map[string]bool{"extension.json": true}
	if manifest.Exec == "" {
		return required
	}
	resolved, local := extensions.ResolveExecPath(dir, manifest.Exec)
	if !local || filepath.IsAbs(filepath.FromSlash(manifest.Exec)) {
		return required
	}
	rel, err := filepath.Rel(dir, resolved)
	if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		required[filepath.ToSlash(rel)] = true
	}
	return required
}

func validateExtensionExecutable(dir string, manifest extensions.Manifest) error {
	if manifest.Exec == "" {
		return nil
	}
	path, local := extensions.ResolveExecPath(dir, manifest.Exec)
	if !local {
		// Bare commands such as "go", "node", and "npx" are resolved
		// from PATH when zut starts. They may be intentionally installed
		// before the runtime is present on the current machine.
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("extension executable %q is missing; provide it before installing. Local Go extensions can opt in with `zut ext install --build=go <path>`", manifest.Exec)
		}
		return fmt.Errorf("stat extension executable %q: %w", manifest.Exec, err)
	}
	if info.IsDir() {
		return fmt.Errorf("extension executable %q is a directory", manifest.Exec)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("extension executable %q is not executable; run chmod +x on it in the source directory before installing", manifest.Exec)
	}
	return nil
}

func extensionDirs() map[string]string {
	out := map[string]string{}
	if h := ZutHome(); h != "" {
		out["global"] = filepath.Join(h, "extensions")
	}
	if cwd, err := os.Getwd(); err == nil {
		out["project"] = filepath.Join(cwd, ".zut", "extensions")
	}
	return out
}

func findExtensionDir(name string) (string, error) {
	for _, dir := range extensionDirs() {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(filepath.Join(candidate, "extension.json")); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("extension %q not found", name)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// copyDir does a recursive copy of src to dst preserving file mode
// bits. Used by `zut ext install <local-path>`.
//
// Entries matched by the source's root .gitignore are skipped, and
// .git itself is always skipped. This keeps non-portable, regeneratable
// directories (e.g. .venv with hardcoded rpaths, node_modules, target/)
// out of the installed copy so the extension stays functional at its new
// location.
func copyDir(src, dst string) error {
	return copyDirWithRequired(src, dst, nil)
}

func copyDirWithRequired(src, dst string, required map[string]bool) error {
	ig := loadGitignore(src)
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if rel != "." {
			name := filepath.Base(rel)
			if info.IsDir() && name == ".git" {
				return filepath.SkipDir
			}
			if ig.Match(relSlash, info.IsDir()) && !requiredCopyPath(required, relSlash, info.IsDir()) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

func requiredCopyPath(required map[string]bool, rel string, isDir bool) bool {
	if required[rel] {
		return true
	}
	if !isDir {
		return false
	}
	prefix := strings.TrimSuffix(rel, "/") + "/"
	for path := range required {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// gitignore matching lives in packages/ignore so the @-file picker in
// packages/agent/modes can share it without an import cycle. These
// thin aliases keep the existing call sites (and tests) terse.
type gitignore = ignore.Gitignore

func loadGitignore(root string) *gitignore { return ignore.Load(root) }

func loadGitignoreFromString(data string) *gitignore { return ignore.Parse(data) }
