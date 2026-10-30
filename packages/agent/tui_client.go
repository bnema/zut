package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/daemonclient"
	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/tui"
)

// detachDeadline bounds the best-effort detach sent when the TUI leaves.
const detachDeadline = 2 * time.Second

// daemonSessionClient adapts daemonclient.Client to modes.SessionClient.
// Detach goes through detacher, which asks the TUI which session it holds,
// so a switch that failed after Attach never releases the wrong session.
type daemonSessionClient struct {
	c *daemonclient.Client
}

var _ modes.SessionClient = (*daemonSessionClient)(nil)

func newDaemonSessionClient(c *daemonclient.Client) *daemonSessionClient {
	return &daemonSessionClient{c: c}
}

func (a *daemonSessionClient) Clear(ctx context.Context, id session.ID) (session.Info, error) {
	return a.c.Clear(ctx, id)
}

func (a *daemonSessionClient) Close(ctx context.Context, id session.ID, reason string) error {
	return a.c.Close(ctx, id, reason)
}

func (a *daemonSessionClient) TelegramBridge(ctx context.Context, sessionID string, on bool) (modes.TelegramBridgeStatus, error) {
	d, err := a.c.TelegramBridge(ctx, session.ID(sessionID), on)
	if err != nil {
		return modes.TelegramBridgeStatus{}, err
	}
	return modes.TelegramBridgeStatus{Active: d.Active, Session: d.Session, Username: d.Username, PairedUserID: d.PairedUserID}, nil
}

func (a *daemonSessionClient) List(ctx context.Context) ([]session.Info, error) {
	return a.c.List(ctx)
}

func (a *daemonSessionClient) Create(ctx context.Context, spec session.CreateSpec) (session.Info, error) {
	// Picker requests carry a path and cwd, not a shell snapshot. Capture
	// the client's environment here rather than borrowing the daemon's.
	// A non-nil environment (including an explicitly empty one) is owned
	// by the caller and must remain unchanged.
	if spec.Env == nil {
		spec.Env = os.Environ()
	}
	return a.c.Create(ctx, spec)
}

// Attach returns a controlling RemotePort for id. Control is taken when the
// TUI subscribes (RemotePort.Subscribe sends the controlling attach). The id
// is validated by the caller's Snapshot (Interactive.Run, /sessions switch),
// which errors for an unknown session.
func (a *daemonSessionClient) Attach(_ context.Context, id session.ID) (session.Port, error) {
	return a.c.Port(id, true), nil
}

// detacher returns a Detach hook that releases the session held reports,
// i.e. the session the TUI actually committed to.
func (a *daemonSessionClient) detacher(held func() session.ID) func() {
	return func() {
		id := held()
		if id == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), detachDeadline)
		defer cancel()
		_ = a.c.Detach(ctx, id)
	}
}

// runTUIClient ensures a daemon, attaches to attachID (or creates a session
// from spec when attachID is empty) and runs the interactive TUI on it.
func runTUIClient(ctx context.Context, attachID session.ID, spec *session.CreateSpec, out io.Writer) int {
	c, err := daemonclient.EnsureDaemon(ctx, daemonclient.EnsureOptions{Options: daemonclient.Options{ClientKind: "tui"}})
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	return runTUIWithClient(ctx, c, attachID, spec, out)
}

// runTUIWithClient owns c and disconnects it on return.
func runTUIWithClient(ctx context.Context, c *daemonclient.Client, attachID session.ID, spec *session.CreateSpec, out io.Writer) int {
	defer c.Disconnect()
	client := newDaemonSessionClient(c)
	if attachID == "" {
		if spec == nil {
			fmt.Fprintln(out, "zut: no session to attach or create")
			return 1
		}
		info, err := client.Create(ctx, *spec)
		if err != nil {
			fmt.Fprintln(out, "zut: create session:", err)
			return 1
		}
		attachID = session.ID(info.ID)
	}
	port, err := client.Attach(ctx, attachID)
	if err != nil {
		fmt.Fprintln(out, "zut: attach:", err)
		return 1
	}
	cfg := tuiClientConfig(out)
	var ui *modes.Interactive
	cfg.Port, cfg.Client = port, client
	cfg.Detach = client.detacher(func() session.ID { return ui.SessionID() })
	ui = modes.NewInteractive(cfg)
	if err := ui.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(out, err)
		return 1
	}
	return 0
}

// tuiClientConfig fills the presentation fields runInteractive derives from
// config and the terminal. Session/domain state comes from the Port.
func tuiClientConfig(out io.Writer) modes.InteractiveConfig {
	cfg, _ := LoadConfig()
	themeEnv := strings.ToLower(strings.TrimSpace(os.Getenv("ZUT_THEME")))
	if themeEnv != "" && themeEnv != "auto" && themeEnv != "dark" && themeEnv != "light" {
		fmt.Fprintf(out, "theme override: unsupported ZUT_THEME=%q; ignoring it\n", themeEnv)
		themeEnv = ""
	}
	pref := tui.ResolveThemePreference(cfg.Theme, themeEnv)
	theme, _, err := tui.DetectThemeWithCustom(ZutHome(), pref.Effective, 80*time.Millisecond)
	if err != nil {
		fmt.Fprintln(out, "theme load:", err)
		if !pref.Forced {
			pref = tui.ResolveThemePreference("", themeEnv)
		}
	}
	return modes.InteractiveConfig{
		Dictation:                 interactiveDictationConfig(cfg.Dictation),
		Terminal:                  tui.NewProcTerm(),
		Theme:                     theme,
		InlineImagesEnabled:       cfg.InlineImagesEnabled,
		TerminalAlertsEnabled:     cfg.TerminalAlertsEnabled,
		TerminalTitleEnabled:      cfg.TerminalTitleEnabled,
		AutoSubagentsEnabled:      cfg.AutoSubagentsEnabled,
		WebSearchEnabled:          cfg.WebSearchEnabled,
		LSPEnabled:                cfg.LSPEnabled,
		SubagentLSPEnabled:        cfg.SubagentLSPEnabled,
		AutoCompactThreshold:      cfg.AutoCompactThreshold,
		GoalMaxTokenBudget:        cfg.Goals.MaxTokenBudget,
		JailByDefault:             cfg.JailByDefault,
		QuickModelShortcuts:       interactiveQuickModelShortcuts(cfg.QuickModelShortcuts),
		ActiveModelProfile:        cfg.ActiveModelProfile,
		RecursiveFileSuggest:      cfg.RecursiveFileSuggest,
		RespectGitignore:          cfg.RespectGitignore,
		CompactMode:               cfg.CompactMode,
		TUIInputStyle:             cfg.TUIInputStyle,
		TUIStatusPosition:         cfg.TUIStatusPosition,
		TUIWorkingPosition:        cfg.TUIWorkingPosition,
		ThemeName:                 cfg.Theme,
		EffectiveThemeName:        pref.Effective,
		ThemeForced:               pref.Forced,
		TerminalProfile:           theme.Terminal,
		FlatTools:                 cfg.FlatToolRender(),
		CompactUser:               cfg.CompactUserInput(),
		ShowInstructionsAtStartup: cfg.ShowInstructionsAtStartup,
		SettingsStore:             configSettingsStore{},
		ZutHome:                   ZutHome(),
	}
}

// runAttachCommand handles `zut attach [id|title-prefix]`. It never spawns a
// daemon.
func runAttachCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) > 1 {
		fmt.Fprintln(stderr, "usage: zut attach [id|title-prefix]")
		return 2
	}
	ctx := context.Background()
	dctx, cancel := context.WithTimeout(ctx, daemonClientDeadline)
	c, err := dialExistingDaemon(dctx)
	if err != nil {
		cancel()
		fmt.Fprintln(stderr, err)
		return 1
	}
	list, err := c.List(dctx)
	cancel()
	if err != nil {
		c.Disconnect()
		fmt.Fprintln(stderr, "zut attach:", err)
		return 1
	}
	var id string
	if len(args) == 1 {
		id, err = resolveAttachTarget(list, args[0])
	} else {
		cwd, _ := os.Getwd()
		id, err = latestDetachedInCWD(list, cwd)
		if err != nil {
			c.Disconnect()
			fmt.Fprintln(stderr, "zut attach:", err)
			_ = runLsCommand(nil, stdout, stderr)
			return 1
		}
	}
	if err != nil {
		c.Disconnect()
		fmt.Fprintln(stderr, "zut attach:", err)
		return 1
	}
	return runTUIWithClient(ctx, c, session.ID(id), nil, stderr)
}

// resolveAttachTarget matches an exact ID, a unique ID prefix, then a unique
// title prefix.
func resolveAttachTarget(list []session.Info, arg string) (string, error) {
	id, idErr := resolveSessionID(list, arg)
	if idErr == nil {
		return id, nil
	}
	if errors.Is(idErr, errSessionAmbiguous) {
		return "", idErr
	}
	var matches []string
	for _, in := range list {
		if in.Title != "" && strings.HasPrefix(in.Title, arg) {
			matches = append(matches, in.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w %q", errSessionNotFound, arg)
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)
		return "", fmt.Errorf("session title prefix %q is %w: %s", arg, errSessionAmbiguous, strings.Join(matches, ", "))
	}
}

// latestDetachedInCWD picks the most recently updated detached session whose
// CWD equals cwd.
func latestDetachedInCWD(list []session.Info, cwd string) (string, error) {
	var best *session.Info
	for i := range list {
		in := &list[i]
		if in.Attach != session.AttachStateDetached || in.CWD != cwd {
			continue
		}
		if best == nil || in.UpdatedAt.After(best.UpdatedAt) {
			best = in
		}
	}
	if best == nil {
		return "", fmt.Errorf("no detached session in %s", cwd)
	}
	return best.ID, nil
}
