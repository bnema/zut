// Package extensions implements the host side of zut's subprocess
// extension protocol. The Manager discovers extensions in well-known
// directories, spawns each one, completes the hello handshake, and
// routes slash commands to the right extension.
//
// Each extension is its own process, communicating with zut over its
// stdin/stdout in newline-delimited JSON. Stderr is redirected to a
// per-extension log file under $ZUT_HOME/logs/. Crashing one
// extension does not affect the others or the host.
//
// See docs/extensions.md for the user-facing reference and
// packages/agent/extproto for the wire format.
package extensions

import (
	"bufio"
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
	"sync"
	"syscall"
	"time"

	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/tui"
)

// Manifest is the extension.json file shipped alongside an
// extension's executable. It tells zut how to launch the extension
// and provides display metadata.
type Manifest struct {
	Name        string   `json:"name"`
	Version     string   `json:"version,omitempty"`
	Exec        string   `json:"exec"`               // executable path, relative to manifest dir
	Args        []string `json:"args,omitempty"`     // extra argv passed to exec
	Language    string   `json:"language,omitempty"` // informational ("go", "python", "typescript", ...)
	Enabled     *bool    `json:"enabled,omitempty"`  // nil = enabled
	Description string   `json:"description,omitempty"`
	Skills      []string `json:"skills,omitempty"`   // bundled skill directories
	HostEnv     []string `json:"host_env,omitempty"` // explicitly inherited host variables
}

// IsEnabled returns the manifest's effective enabled state. Default
// is true so adding a new extension folder Just Works without an
// extra zut ext enable command.
func (m Manifest) IsEnabled() bool {
	return m.Enabled == nil || *m.Enabled
}

func loadManifestSkills(extDir string, mf Manifest) ([]*skills.Skill, []error) {
	if len(mf.Skills) == 0 {
		return nil, nil
	}
	root, err := filepath.Abs(extDir)
	if err != nil {
		return nil, []error{fmt.Errorf("resolve extension directory: %w", err)}
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, []error{fmt.Errorf("resolve extension directory: %w", err)}
	}
	declared := make([]skills.BundledSkillDir, 0, len(mf.Skills))
	var errs []error
	for _, raw := range mf.Skills {
		rel := strings.TrimSpace(raw)
		if rel == "" || filepath.IsAbs(rel) {
			errs = append(errs, fmt.Errorf("skill path %q must be a relative directory", raw))
			continue
		}
		clean := filepath.Clean(rel)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			errs = append(errs, fmt.Errorf("skill path %q escapes the extension directory", raw))
			continue
		}
		candidate := filepath.Join(root, clean)
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			errs = append(errs, fmt.Errorf("skill path %q: %w", raw, err))
			continue
		}
		relResolved, err := filepath.Rel(root, resolved)
		if err != nil || relResolved == ".." || strings.HasPrefix(relResolved, ".."+string(filepath.Separator)) {
			errs = append(errs, fmt.Errorf("skill path %q escapes the extension directory through a symlink", raw))
			continue
		}
		declared = append(declared, skills.BundledSkillDir{Dir: resolved, Root: root, Source: "extension " + mf.Name})
	}
	loaded, loadErrs := skills.LoadBundled(declared)
	return loaded, append(errs, loadErrs...)
}

// Extension is a running extension subprocess and the metadata zut
// tracks about it.
type Extension struct {
	Manifest Manifest
	Dir      string // absolute path to extension directory
	LogPath  string

	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	logFile  *boundedLog
	helloAck bool
	commands []extproto.RegisterCommandFromExt
	tools    []extproto.RegisterToolFromExt
	skills   []*skills.Skill

	writeMu sync.Mutex // serializes concurrent host→extension frames

	// lifecycleFrames decouples session-event delivery from the host's
	// synchronous control paths. The queue preserves lifecycle ordering while
	// bounding backpressure from an extension that stops reading stdin.
	lifecycleFrames   chan []byte
	lifecycleStop     chan struct{}
	lifecycleStopOnce sync.Once

	// readyCh is closed when the extension sends a ReadyFromExt
	// frame, or when the host gives up waiting (registrationGrace).
	readyCh   chan struct{}
	readyOnce sync.Once

	// pending command invocations waiting on a CommandResponseFromExt
	// keyed by the id we sent in CommandInvokedFromHost.
	// pendingTool is the same idea for tool calls.
	// pendingIntercept is the same idea for event_intercept calls.
	mu               sync.Mutex
	pending          map[string]chan extproto.CommandResponseFromExt
	pendingTool      map[string]chan extproto.ToolResultFromExt
	pendingIntercept map[string]chan extproto.EventInterceptResponseFromExt

	// lastFrameTime is updated by the read loop on every frame it
	// processes. Used by the auto-ready idle watchdog so legacy
	// extensions (no `ready` frame) don't pin the WaitForReady wait
	// to its full grace.
	lastFrameTime time.Time

	// eventSubs and interceptSubs are the sets of event names this
	// extension subscribed to via SubscribeFromExt. Used by
	// EmitEvent / InterceptToolCall to filter recipients.
	eventSubs     map[string]struct{}
	interceptSubs map[string]struct{}

	autoReady     bool
	readyTimedOut bool
	diagnostics   []string
}

// HostHooks is the small interface the manager calls back into the
// running TUI through. Decouples extensions from interactive.go.
type HostHooks interface {
	// Notify pushes an ext-originated status message into the chat.
	// Level is one of "info", "warn", "error", "success".
	Notify(extName, level, message string)

	// Submit feeds text as if the user had typed and pressed enter,
	// running it through the agent loop.
	Submit(text string)

	// SubmitSlash runs text as a slash command in the TUI as if the
	// user had typed it (text must start with '/'). Unlike Submit it
	// does NOT run text through the model. Wired to the spontaneous
	// submit_slash frame from extensions; ignored when the host is
	// not interactive.
	SubmitSlash(text string)

	// Insert places text at the cursor in the editor.
	Insert(text string)

	// Display appends a one-shot styled note to the chat without
	// invoking the model and without writing to the transcript.
	Display(extName, text string)

	// ClearNotes removes any notes previously pushed by extName via
	// Notify/Display so transient status lines do not stack forever.
	ClearNotes(extName string)

	OpenPanel(extName string, spec extproto.PanelSpec)
	UpdatePanel(extName, panelID, title string, lines []string, footer string)
	ClosePanel(extName, panelID string)

	// SetStatus and SetWidget maintain persistent extension-owned UI state.
	// Hosts without a persistent UI may ignore these calls.
	SetStatus(extName, key, level, text string)
	SetWidget(extName, id, position, title string, lines []string)
	ClearWidget(extName, id string)
}

// AlertHostHooks is an optional extension to HostHooks. Hosts that support
// structured alerts implement it without breaking older embedders that only
// implement the original HostHooks surface.
type AlertHostHooks interface {
	Alert(extName string, alert extproto.AlertRequest)
}

// ExtensionChromeHooks is an optional lifecycle hook for hosts that retain
// persistent extension-owned UI. The manager calls it when an extension exits
// so a crash, disable, or reload cannot leave stale widgets or statuses.
type ExtensionChromeHooks interface {
	ClearExtensionChrome(extName string)
}

type commandRegistration struct {
	ext  *Extension
	name string
}

// Manager owns every extension subprocess for the lifetime of zut.
type Manager struct {
	zutHome    string
	cwd        string
	zutVersion string
	provider   string
	model      string
	hooks      HostHooks

	mu  sync.RWMutex
	ext map[string]*Extension // keyed by manifest name

	// commandIndex maps a lower-cased slash-command name (without the
	// leading /) to its canonical registration. First-come-first-served:
	// later registrations that differ only by case are dropped with a warning.
	commandIndex map[string]commandRegistration

	// toolIndex maps an extension-defined tool name to its owning
	// extension. Same first-come-first-served rule as commandIndex.
	toolIndex map[string]*Extension

	// explicitPaths remembers ad-hoc paths passed via --ext so
	// Reload can respawn them alongside the discovered set.
	explicitPaths []string

	// onReload, if set, is invoked after a successful Reload. Used
	// by the host so it can rebuild the agent's tool registry with
	// the freshly-registered extension tools.
	onReload func()

	// activeSession is replayed to extensions loaded after the current
	// session was opened, such as after /reload-ext.
	activeSession *sessionEventState
	sessionEmitMu sync.Mutex
}

type sessionEventState struct {
	event  extproto.EventFromHost
	states map[string]json.RawMessage
}

// New constructs an empty Manager. Call Discover to populate it from
// the on-disk extension directories.
func New(zutHome, cwd, zutVersion, provider, model string, hooks HostHooks) *Manager {
	return &Manager{
		zutHome:      zutHome,
		cwd:          cwd,
		zutVersion:   zutVersion,
		provider:     provider,
		model:        model,
		hooks:        hooks,
		ext:          map[string]*Extension{},
		commandIndex: map[string]commandRegistration{},
		toolIndex:    map[string]*Extension{},
	}
}

// Discover scans the global and project extension dirs and starts
// every extension whose manifest is enabled. Spawns happen in
// parallel so a slow runtime (e.g. `npx tsx` cold-start, ~1.5s)
// doesn't block other extensions from starting. Returns a slice of
// errors encountered (one per extension); a single bad extension
// doesn't abort the rest.
func (m *Manager) Discover(ctx context.Context) []error {
	type loadJob struct {
		dir string
	}
	var jobs []loadJob
	seenDirs := map[string]bool{} // dedup by basename so project wins
	for _, dir := range m.searchDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // missing directory is fine
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if seenDirs[e.Name()] {
				continue // higher-priority location already queued
			}
			seenDirs[e.Name()] = true
			jobs = append(jobs, loadJob{dir: filepath.Join(dir, e.Name())})
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(jobs))
	for _, j := range jobs {
		wg.Add(1)
		go func(extDir string) {
			defer wg.Done()
			if err := m.loadOne(ctx, extDir); err != nil {
				errCh <- fmt.Errorf("%s: %w", extDir, err)
			}
		}(j.dir)
	}
	wg.Wait()
	close(errCh)

	var errs []error
	for e := range errCh {
		errs = append(errs, e)
	}
	return errs
}

// searchDirs returns the directories the discoverer walks, in
// priority order: project-local first (so a project can override
// global behavior), then global.
func (m *Manager) searchDirs() []string {
	var dirs []string
	if m.cwd != "" {
		dirs = append(dirs, filepath.Join(m.cwd, ".zut", "extensions"))
	}
	if m.zutHome != "" {
		dirs = append(dirs, filepath.Join(m.zutHome, "extensions"))
	}
	return dirs
}

// loadOne reads a single extension's manifest and, if enabled,
// spawns its subprocess + completes the hello handshake.
func (m *Manager) loadOne(ctx context.Context, dir string) error {
	manifestPath := filepath.Join(dir, "extension.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var mf Manifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if mf.Name == "" {
		return errors.New("manifest: name is required")
	}
	if err := validateExtensionName(mf.Name); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	hasTheme := HasExtensionTheme(dir)
	if mf.Exec == "" && !hasTheme {
		return errors.New("manifest: exec is required")
	}
	if !mf.IsEnabled() {
		// Quietly skip disabled extensions; zut ext list will show them.
		return nil
	}

	m.mu.RLock()
	_, dup := m.ext[mf.Name]
	m.mu.RUnlock()
	if dup {
		// Project-local copy already won; ignore the global one.
		return nil
	}

	bundledSkills, skillErrs := loadManifestSkills(dir, mf)
	ext := &Extension{
		Manifest:         mf,
		Dir:              dir,
		readyCh:          make(chan struct{}),
		pending:          map[string]chan extproto.CommandResponseFromExt{},
		pendingTool:      map[string]chan extproto.ToolResultFromExt{},
		pendingIntercept: map[string]chan extproto.EventInterceptResponseFromExt{},
		eventSubs:        map[string]struct{}{},
		interceptSubs:    map[string]struct{}{},
		skills:           bundledSkills,
		lifecycleFrames:  make(chan []byte, 64),
		lifecycleStop:    make(chan struct{}),
	}
	for _, skillErr := range skillErrs {
		ext.diagnostics = appendBoundedDiagnostic(ext.diagnostics, "skill: "+skillErr.Error())
	}
	if mf.Exec != "" {
		if err := m.spawn(ctx, ext); err != nil {
			return err
		}
	} else {
		ext.readyOnce.Do(func() { close(ext.readyCh) })
	}

	m.mu.Lock()
	m.ext[mf.Name] = ext
	// Note: ext.commands and ext.tools may be empty here — they're
	// populated by the read loop as register_* frames arrive after
	// hello. Indexing happens in the read loop too. Discover()'s
	// caller can WaitForReady() before relying on the registries.
	m.mu.Unlock()
	return nil
}

// LoadExplicit loads each path as an ad-hoc extension. Used for
// `zut --ext <path>` so extension authors can iterate on a working
// copy without having to `zut ext install` after every change.
//
// Loaded BEFORE Discover so explicit paths win on name conflicts
// against installed extensions. Spawns happen in parallel like the
// regular discovery path; errors are returned per path.
// HasExtensionTheme reports whether an extension directory contains a theme
// file in one of the supported extension-owned locations.
func HasExtensionTheme(dir string) bool {
	for _, file := range []string{"theme.json", filepath.Join("themes", "theme.json")} {
		if _, err := os.Stat(filepath.Join(dir, file)); err == nil {
			return true
		}
	}
	return false
}

func (m *Manager) ThemeOptions() []tui.ThemeOption {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.ext))
	for name := range m.ext {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []tui.ThemeOption
	for _, name := range names {
		ext := m.ext[name]
		for _, file := range []string{"theme.json", "themes/theme.json"} {
			path := filepath.Join(ext.Dir, file)
			value := path
			if opt, ok := tui.ThemeOptionFromFile(path, value, "extension "+ext.Manifest.Name); ok {
				out = append(out, opt)
			}
		}
	}
	return out
}

func (m *Manager) LoadExplicit(ctx context.Context, paths []string) []error {
	if len(paths) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(paths))
	absPaths := make([]string, 0, len(paths))
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			errCh <- fmt.Errorf("%s: %w", p, err)
			continue
		}
		absPaths = append(absPaths, abs)
		wg.Add(1)
		go func(extDir string) {
			defer wg.Done()
			if err := m.loadOne(ctx, extDir); err != nil {
				errCh <- fmt.Errorf("%s: %w", extDir, err)
			}
		}(abs)
	}
	wg.Wait()
	close(errCh)

	m.mu.Lock()
	m.explicitPaths = append(m.explicitPaths, absPaths...)
	m.mu.Unlock()

	var errs []error
	for e := range errCh {
		errs = append(errs, e)
	}
	return errs
}

// SetOnReload registers a callback fired after a successful Reload.
// Hosts use it to rebuild the agent's tool registry with freshly-
// registered extension tools.
func (m *Manager) SetOnReload(fn func()) {
	m.mu.Lock()
	m.onReload = fn
	m.mu.Unlock()
}

// ReloadStats summarises the outcome of Reload.
type ReloadStats struct {
	Stopped int     // how many old processes were torn down
	Loaded  int     // how many new processes reached spawn
	Ready   int     // how many of those signalled ready in time
	Errors  []error // non-fatal per-extension errors
}

// Reload tears down every running extension, re-reads the manifests
// from disk, respawns everyone (including the --ext paths remembered
// from LoadExplicit), waits up to grace for ready signals, and
// invokes the SetOnReload callback so the host can rebuild its tool
// registry. The manager's internal maps are cleared before the
// new load to ensure a clean slate.
//
// Safe to call concurrently with normal host operations: the lock is
// released between stop and respawn so pending InvokeTool / Invoke
// calls on the old processes get a clean error as their stdin
// closes.
func (m *Manager) Reload(ctx context.Context, grace time.Duration) ReloadStats {
	stats := ReloadStats{}

	// Snapshot and remember the explicit paths before we wipe state.
	m.mu.Lock()
	old := m.ext
	explicit := append([]string(nil), m.explicitPaths...)
	stats.Stopped = len(old)
	m.ext = map[string]*Extension{}
	m.commandIndex = map[string]commandRegistration{}
	m.toolIndex = map[string]*Extension{}
	m.explicitPaths = nil
	callback := m.onReload
	m.mu.Unlock()

	// Graceful stop of the old set (reuses Stop's shutdown logic,
	// but Stop re-reads m.ext which is now empty, so we replicate
	// the small shutdown loop here on the snapshot).
	oldExts := make([]*Extension, 0, len(old))
	for _, ext := range old {
		oldExts = append(oldExts, ext)
	}
	stopExtensions(oldExts, grace)

	// Fresh load. Explicit paths first (they still win on conflict).
	if errs := m.LoadExplicit(ctx, explicit); len(errs) > 0 {
		stats.Errors = append(stats.Errors, errs...)
	}
	if errs := m.Discover(ctx); len(errs) > 0 {
		stats.Errors = append(stats.Errors, errs...)
	}

	m.mu.RLock()
	stats.Loaded = len(m.ext)
	m.mu.RUnlock()

	// Wait for ready frames. Use the same 3s grace zut uses at
	// startup so the reload feels no slower than a cold boot.
	readyDeadline := time.Now().Add(grace)
	if time.Until(readyDeadline) < 3*time.Second {
		readyDeadline = time.Now().Add(3 * time.Second)
	}
	m.WaitForReady(time.Until(readyDeadline))

	m.mu.RLock()
	for _, ext := range m.ext {
		select {
		case <-ext.readyCh:
			stats.Ready++
		default:
		}
	}
	m.mu.RUnlock()

	if callback != nil {
		callback()
	}

	return stats
}

// WaitForReady blocks until every loaded extension has signalled
// ReadyFromExt, or the grace period expires for the slowest one.
//
// Waits run in parallel: total time is max(per-extension wait), not
// sum. Without this, a single slow extension (e.g. `npx tsx` cold)
// would gate every other extension's wait too and zut startup would
// scale linearly with the number of slow runtimes installed.
//
// Call after Discover and before relying on tool registrations.
func (m *Manager) WaitForReady(grace time.Duration) {
	m.mu.RLock()
	exts := make([]*Extension, 0, len(m.ext))
	for _, e := range m.ext {
		exts = append(exts, e)
	}
	m.mu.RUnlock()

	deadline := time.After(grace)
	var wg sync.WaitGroup
	for _, ext := range exts {
		wg.Add(1)
		go func(ext *Extension) {
			defer wg.Done()
			select {
			case <-ext.readyCh:
			case <-deadline:
				ext.mu.Lock()
				ext.readyTimedOut = true
				ext.diagnostics = appendBoundedDiagnostic(ext.diagnostics, "timed out waiting for ready frame")
				ext.mu.Unlock()
				fmt.Fprintf(ext.logFile, "[zut] timed out waiting for ready frame; proceeding\n")
				ext.readyOnce.Do(func() { close(ext.readyCh) })
			}
		}(ext)
	}
	wg.Wait()
}

// extensionExitGrace bounds how long startup waits for a process to exit
// after it closes stdout before completing the hello handshake.
const extensionExitGrace = time.Second

// ResolveExecPath applies the manifest exec resolution rules. The bool is
// true when exec names an absolute or extension-relative path; false means
// the value is a bare command resolved through PATH.
func ResolveExecPath(extDir, execName string) (string, bool) {
	normalized := filepath.FromSlash(execName)
	if filepath.IsAbs(normalized) {
		return normalized, true
	}
	if normalized == "." || normalized == ".." ||
		strings.HasPrefix(normalized, "."+string(filepath.Separator)) ||
		strings.HasPrefix(normalized, ".."+string(filepath.Separator)) ||
		strings.ContainsRune(normalized, filepath.Separator) {
		return filepath.Join(extDir, normalized), true
	}
	return normalized, false
}

// spawn launches the subprocess, hooks up pipes, logs stderr, and
// runs the synchronous portion of the hello handshake. Asynchronous
// frames are processed in a goroutine started here.
func (m *Manager) spawn(ctx context.Context, ext *Extension) error {
	logsDir := filepath.Join(m.zutHome, "logs")
	_ = os.MkdirAll(logsDir, 0o755)
	logPath := filepath.Join(logsDir, "ext-"+ext.Manifest.Name+".log")
	logFile, err := openBoundedLog(logPath)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	ext.LogPath = logPath
	ext.logFile = logFile
	fmt.Fprintf(logFile, "\n[zut] starting %s/%s at %s\n", ext.Manifest.Name, ext.Manifest.Version, time.Now().Format(time.RFC3339))

	// Bare names (node, npx, python3, tsx, etc.) are looked up via PATH;
	// absolute and extension-relative paths are resolved by the helper.
	execPath, _ := ResolveExecPath(ext.Dir, ext.Manifest.Exec)
	env, err := extensionEnvironment(ext.Manifest.HostEnv, os.LookupEnv)
	if err != nil {
		_ = logFile.Close()
		return fmt.Errorf("manifest host_env: %w", err)
	}
	if runtime.GOOS != "windows" {
		foundPWD := false
		for index, value := range env {
			if strings.HasPrefix(value, "PWD=") {
				env[index] = "PWD=" + ext.Dir
				foundPWD = true
				break
			}
		}
		if !foundPWD {
			env = append(env, "PWD="+ext.Dir)
		}
	}
	cmd := exec.CommandContext(ctx, execPath, ext.Manifest.Args...)
	cmd.Dir = ext.Dir
	cmd.Env = env
	cmd.Stderr = logFile
	isolateExtensionProcess(cmd)

	var stdin io.WriteCloser
	var stdout io.ReadCloser
	started := false
	cleanup := true
	defer func() {
		if !cleanup {
			return
		}
		if stdin != nil {
			_ = stdin.Close()
		}
		if stdout != nil {
			_ = stdout.Close()
		}
		if started {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		_ = logFile.Close()
	}()

	stdin, err = cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe (stderr log: %s): %w", logPath, err)
	}
	stdout, err = cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe (stderr log: %s): %w", logPath, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn (stderr log: %s): %w", logPath, err)
	}
	started = true
	ext.cmd = cmd
	ext.stdin = stdin
	ext.stdout = stdout

	// Hello handshake. Read the extension's HelloFromExt synchronously
	// so we can fail fast on a broken extension; everything after is
	// processed in the read goroutine.
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read hello (stderr log: %s): %w", logPath, err)
		}
		status, exited := waitForExtensionExit(cmd, extensionExitGrace)
		started = false // waitForExtensionExit always reaps the process.
		if exited {
			return fmt.Errorf("extension exited before hello: %s (stderr log: %s)", status, logPath)
		}
		return fmt.Errorf("extension closed stdout before hello without exiting (stderr log: %s)", logPath)
	}
	var hello extproto.HelloFromExt
	if err := json.Unmarshal(scanner.Bytes(), &hello); err != nil {
		return fmt.Errorf("parse hello (stderr log: %s): %w", logPath, err)
	}
	if hello.Type != "hello" || hello.Name == "" {
		return fmt.Errorf("first frame must be hello (got %q; stderr log: %s)", hello.Type, logPath)
	}
	if hello.ProtocolVersion != extproto.ProtocolVersion {
		return fmt.Errorf("unsupported extension protocol version %d (want %d; stderr log: %s)", hello.ProtocolVersion, extproto.ProtocolVersion, logPath)
	}
	// Trust the manifest's name; ignore mismatch from the hello.
	ext.helloAck = true

	m.mu.RLock()
	var activeSession *extproto.SessionContext
	if m.activeSession != nil {
		activeSession = cloneSessionContext(m.activeSession.event.Session)
	}
	m.mu.RUnlock()
	ack, _ := extproto.Encode(extproto.HelloAckFromHost{
		Type:            "hello_ack",
		ProtocolVersion: extproto.ProtocolVersion,
		ZutVersion:      m.zutVersion,
		Provider:        m.provider,
		Model:           m.model,
		CWD:             m.cwd,
		ExtensionDir:    ext.Dir,
		DataDir:         ext.Dir,
		Session:         activeSession,
	})
	if _, err := stdin.Write(ack); err != nil {
		return fmt.Errorf("send hello_ack (stderr log: %s): %w", logPath, err)
	}

	cleanup = false

	// Spin up the lifecycle writer and read loop now that the handshake is done.
	ext.startLifecycleWriter()
	go m.readLoop(ext, scanner)

	// Compatibility shim: extensions built against the phase-1 SDK
	// don't send a ready frame. Watch the read loop's frame arrival
	// rate; if nothing's arrived for readyIdleWindow we treat the
	// extension as ready so WaitForReady doesn't burn the full grace
	// on every startup. Newer extensions still trigger the explicit
	// path on their own ready frame.
	go m.assumeReadyAfterIdle(ext)

	return nil
}

// waitForExtensionExit returns the process status when it exits promptly.
// A process that closes stdout but remains alive has already violated the
// protocol, so kill and reap it rather than blocking extension discovery.
func waitForExtensionExit(cmd *exec.Cmd, grace time.Duration) (string, bool) {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-done:
		if cmd.ProcessState != nil {
			return cmd.ProcessState.String(), true
		}
		if err != nil {
			return err.Error(), true
		}
		return "exit status 0", true
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-done
		return "", false
	}
}

// readyIdleWindow is how long the manager waits for a frame after
// hello before assuming an extension that doesn't send `ready` is
// nevertheless ready. 250ms is enough for any well-behaved native
// binary to flush its register frames; slow runtimes (npx tsx) flush
// even faster once they've started, so this rarely affects them.
const readyIdleWindow = 250 * time.Millisecond

func (m *Manager) assumeReadyAfterIdle(ext *Extension) {
	ext.mu.Lock()
	last := ext.lastFrameTime
	ext.mu.Unlock()
	for {
		select {
		case <-ext.readyCh:
			return
		case <-time.After(readyIdleWindow):
		}
		ext.mu.Lock()
		current := ext.lastFrameTime
		ext.mu.Unlock()
		if current.Equal(last) {
			// No new frame in the idle window. Treat as ready.
			ext.readyOnce.Do(func() {
				ext.mu.Lock()
				ext.autoReady = true
				ext.diagnostics = appendBoundedDiagnostic(ext.diagnostics, "no ready frame; auto-ready after idle")
				ext.mu.Unlock()
				fmt.Fprintf(ext.logFile, "[zut] no ready frame; auto-readying after idle (legacy SDK?)\n")
				close(ext.readyCh)
			})
			return
		}
		last = current
	}
}

// readLoop processes every frame the extension sends after hello.
// Returns when stdout closes.
func (m *Manager) readLoop(ext *Extension, scanner *bufio.Scanner) {
	defer func() {
		// On close, drop every command + tool this extension owned so
		// future invocations don't dangle. The subprocess is gone; we
		// won't hear back about its commands or tool calls anymore.
		m.mu.Lock()
		for name, registration := range m.commandIndex {
			if registration.ext == ext {
				delete(m.commandIndex, name)
			}
		}
		for name, owner := range m.toolIndex {
			if owner == ext {
				delete(m.toolIndex, name)
			}
		}
		m.mu.Unlock()
		if hooks, ok := m.hooks.(ExtensionChromeHooks); ok {
			hooks.ClearExtensionChrome(ext.Manifest.Name)
		}
		ext.stopLifecycleWriter()
		ext.readyOnce.Do(func() { close(ext.readyCh) })
		fmt.Fprintf(ext.logFile, "[zut] extension %s read loop exited at %s\n", ext.Manifest.Name, time.Now().Format(time.RFC3339))
	}()

	for scanner.Scan() {
		line := scanner.Bytes()
		ext.mu.Lock()
		ext.lastFrameTime = time.Now()
		ext.mu.Unlock()
		var frame extproto.Frame
		if err := json.Unmarshal(line, &frame); err != nil {
			ext.recordDiagnostic(fmt.Sprintf("malformed json frame: %v", err))
			fmt.Fprintf(ext.logFile, "[zut] malformed json from extension: %v\n", err)
			continue
		}
		switch frame.Type {
		case "register_command":
			var rc extproto.RegisterCommandFromExt
			if err := json.Unmarshal(line, &rc); err == nil {
				// Both ext.commands and m.commandIndex are read by
				// the public Commands() / HasCommand() helpers under
				// m.mu, so the writes have to take the same lock to
				// keep the race detector happy.
				m.mu.Lock()
				ext.commands = append(ext.commands, rc)
				key := strings.ToLower(rc.Name)
				_, shadowed := m.commandIndex[key]
				if !shadowed {
					m.commandIndex[key] = commandRegistration{ext: ext, name: rc.Name}
				}
				m.mu.Unlock()
				if shadowed {
					ext.recordDiagnostic(fmt.Sprintf("command /%s conflicts with another extension registering the same name", rc.Name))
				}
			}
		case "register_tool":
			var rt extproto.RegisterToolFromExt
			if err := json.Unmarshal(line, &rt); err != nil {
				fmt.Fprintf(ext.logFile, "[zut] bad register_tool frame: %v\n", err)
				continue
			}
			// Validate the schema parses as JSON. If not, refuse to
			// register — a broken schema confuses the model.
			if len(rt.Schema) > 0 {
				var tmp any
				if err := json.Unmarshal(rt.Schema, &tmp); err != nil {
					ext.recordDiagnostic(fmt.Sprintf("tool %q schema is not valid json: %v", rt.Name, err))
					fmt.Fprintf(ext.logFile, "[zut] tool %q: schema is not valid json (%v); skipped\n", rt.Name, err)
					continue
				}
			}
			m.mu.Lock()
			ext.tools = append(ext.tools, rt)
			_, shadowed := m.toolIndex[rt.Name]
			if !shadowed {
				m.toolIndex[rt.Name] = ext
			}
			m.mu.Unlock()
			if shadowed {
				ext.recordDiagnostic(fmt.Sprintf("tool %s conflicts with another extension registering the same name", rt.Name))
			}
		case "ready":
			ext.readyOnce.Do(func() { close(ext.readyCh) })
			m.replaySessionEvent(ext)
		case "subscribe":
			var sub extproto.SubscribeFromExt
			if err := json.Unmarshal(line, &sub); err == nil {
				ext.mu.Lock()
				for _, ev := range sub.Events {
					ext.eventSubs[ev] = struct{}{}
				}
				for _, ev := range sub.Intercept {
					switch ev {
					case "tool_call", "turn_start", "assistant_message":
						ext.interceptSubs[ev] = struct{}{}
					}
				}
				ext.mu.Unlock()
			}
		case "event_intercept_response":
			var er extproto.EventInterceptResponseFromExt
			if err := json.Unmarshal(line, &er); err == nil {
				ext.mu.Lock()
				ch, ok := ext.pendingIntercept[er.ID]
				if ok {
					delete(ext.pendingIntercept, er.ID)
				}
				ext.mu.Unlock()
				if ok {
					select {
					case ch <- er:
					default:
					}
				}
			}
		case "tool_result":
			var tr extproto.ToolResultFromExt
			if err := json.Unmarshal(line, &tr); err == nil {
				ext.mu.Lock()
				ch, ok := ext.pendingTool[tr.ID]
				if ok {
					delete(ext.pendingTool, tr.ID)
				}
				ext.mu.Unlock()
				if ok {
					select {
					case ch <- tr:
					default:
					}
				}
			}
		case "notify":
			var n extproto.NotifyFromExt
			if err := json.Unmarshal(line, &n); err == nil && m.hooks != nil {
				m.hooks.Notify(ext.Manifest.Name, n.Level, n.Message)
			}
		case "status":
			var s extproto.StatusFromExt
			if err := json.Unmarshal(line, &s); err == nil && m.hooks != nil {
				m.hooks.SetStatus(ext.Manifest.Name, s.Key, s.Level, s.Text)
			}
		case "widget":
			var w extproto.WidgetFromExt
			if err := json.Unmarshal(line, &w); err == nil && m.hooks != nil {
				m.hooks.SetWidget(ext.Manifest.Name, w.ID, w.Position, w.Title, w.Lines)
			}
		case "widget_clear":
			var w extproto.ClearWidgetFromExt
			if err := json.Unmarshal(line, &w); err == nil && m.hooks != nil {
				m.hooks.ClearWidget(ext.Manifest.Name, w.ID)
			}
		case "alert":
			var a extproto.AlertFromExt
			if err := json.Unmarshal(line, &a); err == nil && m.hooks != nil {
				if hooks, ok := m.hooks.(AlertHostHooks); ok {
					hooks.Alert(ext.Manifest.Name, a.AlertRequest)
				}
			}
		case "clear_notes":
			if m.hooks != nil {
				m.hooks.ClearNotes(ext.Manifest.Name)
			}
		case "submit":
			var s extproto.SubmitFromExt
			if err := json.Unmarshal(line, &s); err == nil && m.hooks != nil {
				text := strings.TrimSpace(s.Text)
				if text != "" {
					m.hooks.Submit(text)
				}
			}
		case "submit_slash":
			// Spontaneous request to invoke a slash command in the
			// TUI. Refused unless the payload looks like a slash
			// command so a misbehaving extension can't sneak a model
			// prompt through this path.
			var s extproto.SubmitSlashFromExt
			if err := json.Unmarshal(line, &s); err == nil {
				text := strings.TrimSpace(s.Text)
				if strings.HasPrefix(text, "/") && m.hooks != nil {
					m.hooks.SubmitSlash(text)
				} else {
					fmt.Fprintf(ext.logFile, "[zut] submit_slash refused (not a slash command): %q\n", s.Text)
				}
			}
		case "command_response":
			var cr extproto.CommandResponseFromExt
			if err := json.Unmarshal(line, &cr); err == nil {
				ext.mu.Lock()
				ch, ok := ext.pending[cr.ID]
				if ok {
					delete(ext.pending, cr.ID)
				}
				ext.mu.Unlock()
				if ok {
					select {
					case ch <- cr:
					default:
					}
				}
			}
		case "open_panel":
			var op extproto.OpenPanelFromExt
			if err := json.Unmarshal(line, &op); err == nil && m.hooks != nil {
				m.hooks.OpenPanel(ext.Manifest.Name, op.Panel)
			}
		case "panel_render":
			var pr extproto.PanelRenderFromExt
			if err := json.Unmarshal(line, &pr); err == nil && m.hooks != nil {
				m.hooks.UpdatePanel(ext.Manifest.Name, pr.PanelID, pr.Title, pr.Lines, pr.Footer)
			}
		case "panel_close":
			var pc extproto.PanelCloseFromExt
			if err := json.Unmarshal(line, &pc); err == nil && m.hooks != nil {
				m.hooks.ClosePanel(ext.Manifest.Name, pc.PanelID)
			}
		case "shutdown_ack":
			// Caller of Stop is waiting on the process exit, not this frame.
		default:
			ext.recordDiagnostic(fmt.Sprintf("unknown frame type %q", frame.Type))
			fmt.Fprintf(ext.logFile, "[zut] unknown frame type %q\n", frame.Type)
		}
	}
}

const lifecycleWriteGrace = 250 * time.Millisecond

func (ext *Extension) writeFrame(frame []byte) (int, error) {
	if ext == nil || ext.stdin == nil {
		return 0, fmt.Errorf("extension stdin is closed")
	}
	ext.writeMu.Lock()
	defer ext.writeMu.Unlock()
	return ext.stdin.Write(frame)
}

// tryWriteLifecycleFrame bounds only a lifecycle writer's wait for the shared
// writer lock. Synchronous host frames retain their existing write behavior;
// a stalled lifecycle snapshot is dropped rather than delaying them.
func (ext *Extension) tryWriteLifecycleFrame(frame []byte) error {
	if ext == nil || ext.stdin == nil {
		return fmt.Errorf("extension stdin is closed")
	}
	if !ext.writeMu.TryLock() {
		timer := time.NewTimer(lifecycleWriteGrace)
		defer timer.Stop()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-timer.C:
				return fmt.Errorf("extension lifecycle write is blocked")
			case <-ticker.C:
				if ext.writeMu.TryLock() {
					goto locked
				}
			}
		}
	}
locked:
	defer ext.writeMu.Unlock()
	_, err := ext.stdin.Write(frame)
	return err
}

func (ext *Extension) startLifecycleWriter() {
	if ext.lifecycleFrames == nil {
		ext.lifecycleFrames = make(chan []byte, 64)
	}
	if ext.lifecycleStop == nil {
		ext.lifecycleStop = make(chan struct{})
	}
	go ext.lifecycleWriter()
}

func (ext *Extension) stopLifecycleWriter() {
	if ext == nil || ext.lifecycleStop == nil {
		return
	}
	ext.lifecycleStopOnce.Do(func() { close(ext.lifecycleStop) })
}

func (ext *Extension) enqueueLifecycleFrame(frame []byte) bool {
	if ext == nil {
		return false
	}
	if ext.lifecycleFrames == nil || ext.lifecycleStop == nil {
		_, err := ext.writeFrame(frame)
		return err == nil
	}
	copyFrame := append([]byte(nil), frame...)
	select {
	case <-ext.lifecycleStop:
		return false
	default:
	}
	select {
	case ext.lifecycleFrames <- copyFrame:
		return true
	default:
		// Prefer the newest lifecycle snapshot when a broken extension stops
		// draining its pipe. The latest event still carries the current state.
		select {
		case <-ext.lifecycleFrames:
		default:
		}
		select {
		case ext.lifecycleFrames <- copyFrame:
			return true
		default:
			return false
		}
	}
}

func (ext *Extension) lifecycleWriter() {
	for {
		select {
		case <-ext.lifecycleStop:
			return
		case frame := <-ext.lifecycleFrames:
			if err := ext.tryWriteLifecycleFrame(frame); err != nil {
				return
			}
		}
	}
}

func (ext *Extension) recordDiagnostic(msg string) {
	ext.mu.Lock()
	defer ext.mu.Unlock()
	ext.diagnostics = appendBoundedDiagnostic(ext.diagnostics, msg)
}

// RegisteredCommandDiagnostic describes one command an extension attempted
// to register and whether it owns the active dispatch slot.
type RegisteredCommandDiagnostic struct {
	Name        string
	Description string
	Active      bool
}

// RegisteredToolDiagnostic describes one tool an extension attempted to
// register and whether it owns the active dispatch slot.
type RegisteredToolDiagnostic struct {
	Name        string
	Description string
	Active      bool
}

// ExtensionDiagnostic is a stable snapshot for diagnostic commands.
type ExtensionDiagnostic struct {
	Name          string
	Version       string
	Dir           string
	LogPath       string
	ThemeOnly     bool
	Ready         bool
	AutoReady     bool
	ReadyTimedOut bool
	Commands      []RegisteredCommandDiagnostic
	Tools         []RegisteredToolDiagnostic
	Messages      []string
}

// Diagnostics returns a snapshot of loaded extension state for commands such
// as `zut ext doctor`. It is read-only and does not change manager behavior.
func (m *Manager) Diagnostics() []ExtensionDiagnostic {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.ext))
	for name := range m.ext {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ExtensionDiagnostic, 0, len(names))
	for _, name := range names {
		ext := m.ext[name]
		ext.mu.Lock()
		msgs := append([]string(nil), ext.diagnostics...)
		autoReady := ext.autoReady
		readyTimedOut := ext.readyTimedOut
		ext.mu.Unlock()

		diag := ExtensionDiagnostic{
			Name:          ext.Manifest.Name,
			Version:       ext.Manifest.Version,
			Dir:           ext.Dir,
			LogPath:       ext.LogPath,
			ThemeOnly:     ext.Manifest.Exec == "",
			AutoReady:     autoReady,
			ReadyTimedOut: readyTimedOut,
			Messages:      msgs,
		}
		select {
		case <-ext.readyCh:
			diag.Ready = true
		default:
		}
		for _, c := range ext.commands {
			registration, ok := m.commandIndex[strings.ToLower(c.Name)]
			diag.Commands = append(diag.Commands, RegisteredCommandDiagnostic{
				Name:        c.Name,
				Description: c.Description,
				Active:      ok && registration.ext == ext && registration.name == c.Name,
			})
		}
		for _, t := range ext.tools {
			diag.Tools = append(diag.Tools, RegisteredToolDiagnostic{
				Name:        t.Name,
				Description: t.Description,
				Active:      m.toolIndex[t.Name] == ext,
			})
		}
		out = append(out, diag)
	}
	return out
}

// Commands returns a snapshot of every (extension, command) pair
// currently registered. Used by the slash autocomplete + /help.
func (m *Manager) Commands() []CommandInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []CommandInfo
	for _, ext := range m.ext {
		for _, c := range ext.commands {
			registration, ok := m.commandIndex[strings.ToLower(c.Name)]
			if !ok || registration.ext != ext || registration.name != c.Name {
				continue
			}
			out = append(out, CommandInfo{
				Extension:   ext.Manifest.Name,
				Name:        c.Name,
				Description: c.Description,
			})
		}
	}
	return out
}

// CommandInfo is one extension-registered slash command, surfaced to
// the rest of zut for display purposes.
type CommandInfo struct {
	Extension   string
	Name        string
	Description string
}

// ToolInfo is one extension-registered tool. Used by the agent's
// build step to materialise core.Tool wrappers.
type ToolInfo struct {
	Extension   string
	Name        string
	Description string
	Schema      json.RawMessage
	Deferred    bool
}

// Tools returns a snapshot of every (extension, tool) pair currently
// registered. Used at agent-build time to fold extension tools into
// the runtime tool registry.
func (m *Manager) Tools() []ToolInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []ToolInfo
	for _, ext := range m.ext {
		for _, t := range ext.tools {
			out = append(out, ToolInfo{
				Extension:   ext.Manifest.Name,
				Name:        t.Name,
				Description: t.Description,
				Schema:      t.Schema,
				Deferred:    t.Deferred,
			})
		}
	}
	return out
}

// Skills returns extension-bundled skills in deterministic extension-name
// order. The build layer merges them after user skills and before built-ins.
func (m *Manager) Skills() []*skills.Skill {
	m.mu.RLock()
	names := make([]string, 0, len(m.ext))
	for name := range m.ext {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []*skills.Skill
	for _, name := range names {
		ext := m.ext[name]
		out = append(out, ext.skills...)
	}
	m.mu.RUnlock()
	return out
}

// HasTool reports whether name is registered by any extension.
func (m *Manager) HasTool(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.toolIndex[name]
	return ok
}

// InvokeTool sends a tool_call to the owning extension and waits for
// the matching tool_result. Used by the core.Tool wrapper that the
// agent registers per extension-defined tool.
func (m *Manager) InvokeTool(ctx context.Context, name string, args json.RawMessage, timeout time.Duration) (extproto.ToolResultFromExt, error) {
	m.mu.RLock()
	ext, ok := m.toolIndex[name]
	m.mu.RUnlock()
	if !ok {
		return extproto.ToolResultFromExt{}, fmt.Errorf("no extension registered for tool %q", name)
	}

	id := newCorrelationID()
	ch := make(chan extproto.ToolResultFromExt, 1)
	ext.mu.Lock()
	ext.pendingTool[id] = ch
	ext.mu.Unlock()

	frame, _ := extproto.Encode(extproto.ToolCallFromHost{
		Type: "tool_call",
		ID:   id,
		Name: name,
		Args: args,
	})
	if _, err := ext.writeFrame(frame); err != nil {
		ext.mu.Lock()
		delete(ext.pendingTool, id)
		ext.mu.Unlock()
		return extproto.ToolResultFromExt{}, fmt.Errorf("write: %w", err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		ext.mu.Lock()
		delete(ext.pendingTool, id)
		ext.mu.Unlock()
		return extproto.ToolResultFromExt{}, fmt.Errorf("timeout waiting for %s/%s", ext.Manifest.Name, name)
	case <-ctx.Done():
		ext.mu.Lock()
		delete(ext.pendingTool, id)
		ext.mu.Unlock()
		return extproto.ToolResultFromExt{}, ctx.Err()
	}
}

// HasCommand reports whether name is registered by any extension.
func (m *Manager) HasCommand(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.commandIndex[strings.ToLower(name)]
	return ok
}

func (m *Manager) CommandOwner(name string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if registration, ok := m.commandIndex[strings.ToLower(name)]; ok && registration.ext != nil {
		return registration.ext.Manifest.Name
	}
	return ""
}

// Invoke fires the named slash command's handler in the owning
// extension and waits up to timeout for the response. Returns the
// extension's CommandResponse so the caller can act on the action
// (prompt / insert / display).
func (m *Manager) Invoke(ctx context.Context, name, args string, timeout time.Duration) (extproto.CommandResponseFromExt, error) {
	m.mu.RLock()
	registration, ok := m.commandIndex[strings.ToLower(name)]
	m.mu.RUnlock()
	if !ok {
		return extproto.CommandResponseFromExt{}, fmt.Errorf("no extension registered for /%s", name)
	}
	ext := registration.ext
	name = registration.name

	id := newCorrelationID()
	ch := make(chan extproto.CommandResponseFromExt, 1)
	ext.mu.Lock()
	ext.pending[id] = ch
	ext.mu.Unlock()

	frame, _ := extproto.Encode(extproto.CommandInvokedFromHost{
		Type: "command_invoked",
		ID:   id,
		Name: name,
		Args: args,
	})
	if _, err := ext.writeFrame(frame); err != nil {
		ext.mu.Lock()
		delete(ext.pending, id)
		ext.mu.Unlock()
		return extproto.CommandResponseFromExt{}, fmt.Errorf("write: %w", err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		ext.mu.Lock()
		delete(ext.pending, id)
		ext.mu.Unlock()
		return extproto.CommandResponseFromExt{}, fmt.Errorf("timeout waiting for %s/%s", ext.Manifest.Name, name)
	case <-ctx.Done():
		ext.mu.Lock()
		delete(ext.pending, id)
		ext.mu.Unlock()
		return extproto.CommandResponseFromExt{}, ctx.Err()
	}
}

// Stop cleanly terminates every extension. Sends ShutdownFromHost,
// waits up to gracePeriod for each subprocess to exit, then SIGTERMs
// (and SIGKILLs after another second) the holdouts.
func (m *Manager) SendPanelKey(extName, panelID, key, text string) error {
	m.mu.RLock()
	ext, ok := m.ext[extName]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no extension %q", extName)
	}
	frame, _ := extproto.Encode(extproto.PanelKeyFromHost{Type: "panel_key", PanelID: panelID, Key: key, Text: text})
	_, err := ext.writeFrame(frame)
	return err
}

func (m *Manager) SendPanelClose(extName, panelID string) error {
	m.mu.RLock()
	ext, ok := m.ext[extName]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no extension %q", extName)
	}
	frame, _ := extproto.Encode(extproto.PanelCloseFromHost{Type: "panel_close", PanelID: panelID})
	_, err := ext.writeFrame(frame)
	return err
}

func (m *Manager) Stop(gracePeriod time.Duration) {
	m.mu.RLock()
	exts := make([]*Extension, 0, len(m.ext))
	for _, e := range m.ext {
		exts = append(exts, e)
	}
	m.mu.RUnlock()
	stopExtensions(exts, gracePeriod)
}

func stopExtensions(exts []*Extension, gracePeriod time.Duration) {
	for _, ext := range exts {
		if ext.stdin == nil {
			continue
		}
		if frame, err := extproto.Encode(extproto.ShutdownFromHost{Type: "shutdown"}); err == nil {
			_, _ = ext.writeFrame(frame)
		}
		_ = ext.stdin.Close()
	}

	deadline := time.Now().Add(gracePeriod)
	for _, ext := range exts {
		if ext.cmd == nil {
			if ext.logFile != nil {
				_ = ext.logFile.Close()
			}
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			remaining = 100 * time.Millisecond
		}
		done := make(chan struct{})
		go func() { _ = ext.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(remaining):
			_ = ext.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(time.Second):
				_ = ext.cmd.Process.Kill()
				<-done
			}
		}
		if ext.logFile != nil {
			_ = ext.logFile.Close()
		}
	}
}

// All returns every extension currently tracked, enabled or not.
// Used by `zut ext list`.
func (m *Manager) All() []*Extension {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Extension, 0, len(m.ext))
	for _, e := range m.ext {
		out = append(out, e)
	}
	return out
}

// newCorrelationID returns a short non-cryptographic id. We don't
// need uniqueness across processes, just within the lifetime of one
// extension's pending map.
func newCorrelationID() string {
	return strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
}
