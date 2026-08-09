package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/tui"
	"github.com/google/uuid"
	"golang.org/x/term"
)

// Mode is the CLI run mode.
type Mode string

const (
	ModeInteractive Mode = "interactive"
	ModePrint       Mode = "print"
	ModeStream      Mode = "stream"
	ModeJSON        Mode = "json"
	ModeRPC         Mode = "rpc"
	// ModeSubagentWorker is the long-lived, headless daemon mode used by
	// subagent-spawned agents. The binary opens a unix-socket inbox at
	// the path provided by --subagent-worker, reads versioned JSONL
	// supervisor messages, runs each user turn against a persistent session,
	// and streams JSONL events on
	// stdout.
	ModeSubagentWorker Mode = "subagent-worker"
)

// Args holds parsed command-line options.
type Args struct {
	Mode        Mode
	Orchestrate bool
	Provider    string
	Model       string
	APIKey      string

	// inheritedCredential is populated only by subagent-worker mode from
	// the supervisor's stdin. It is never accepted as a CLI argument.
	inheritedCredential string
	inheritedAuthMethod string
	inheritedAccountID  string

	BaseURL            string // override provider base URL (for tests/self-hosted)
	SystemPrompt       string
	AppendSystemPrompt []string
	Reasoning          string
	Temperature        *float32

	// FastMode is an internal subagent-child propagation flag. Normal users
	// enable fast mode through the persisted config /settings toggle.
	FastMode bool
	// FastModeSet distinguishes an explicit child override (including
	// --no-fast-mode) from an omitted flag so persisted subagent settings
	// survive a parent config change.
	FastModeSet bool

	Continue        bool
	Resume          bool
	ResumeSessionID string
	Session         string
	NoSess          bool

	CWD     string
	NoTools bool
	NoLSP   bool
	Tools   []string
	// ToolsSet distinguishes an omitted --tools flag from an explicitly
	// supplied empty list. Other built-in tools intentionally retain their
	// historical empty-list behavior; web search uses this provenance as a
	// capability allowlist.
	ToolsSet bool

	// WebSearchPolicy is an internal capability override. Normal CLI args
	// leave it at Inherit so Resolve applies config and --tools provenance;
	// SDK, bot, and subagent-worker callers set it explicitly.
	WebSearchPolicy subagents.WebSearchPolicy
	MaxSteps        int

	// Exts is a list of directory paths the user passed via --ext.
	// Each must contain an extension.json. Loaded for one session
	// only; never persisted. Take precedence over installed exts of
	// the same name.
	Exts []string

	// NoExt disables extension discovery + spawn entirely for this
	// run. --ext PATH still works (explicit beats implicit) so you
	// can run "with only this one extension" via --no-ext --ext PATH.
	NoExt bool

	// NoSkill disables ALL skill discovery for this run, including
	// the built-in skills compiled into the binary. The system
	// prompt loses its "Available skills" manifest and the `skill`
	// tool isn't registered. Useful for running zut without any
	// extra context biasing the model.
	NoSkill bool

	// WithSkills controls loading user-installed skills from
	// $ZUT_HOME/skills/, .zut/skills/, .claude/skills/, and
	// .agents/skills/. It defaults to true; --no-skill disables all
	// skill discovery, including built-ins.
	WithSkills bool

	// InsecureTLS skips TLS verification for custom inference endpoints.
	InsecureTLS bool

	// NoYolo turns on per-tool confirmation. Before each tool
	// invocation the TUI prompts the user with the tool name + args
	// and waits for an explicit yes/no. The user can also pick
	// "always for this tool this session" or "always for anything
	// this session" to stop being prompted again. Defaults off
	// (yolo mode): tools run without asking.
	//
	// No effect in -p / --json / rpc modes, which have no
	// interactive prompt. A warning is printed to stderr on startup
	// so scripts know the flag is ignored, but tools still run
	// freely so automated workflows keep working.
	NoYolo bool

	// Yes accepts zutfile launch consent without an interactive
	// Allow? prompt (zut run -y / --yes). Durable consent receipts
	// are still written for modes other than bash ask.
	Yes bool

	ListModels bool
	Help       bool
	Version    bool
	StatsPath  string // write print-mode generation statistics as JSON

	Prompt string // concatenated positional args

	// StartupPre is an optional zutfile entry.pre value. Interactive
	// mode auto-submits it once at startup before InitialInput handling.
	StartupPre string

	// SubagentWorker is the inbox-socket path when this process is a
	// subagent worker. Empty in every other mode. Set by either
	// --subagent-worker.
	SubagentWorker string

	// SubagentMaxTurns limits message turns in one worker run.
	SubagentMaxTurns int
	// SubagentTurnTimeout limits each active delegated turn while leaving the
	// worker process alive between turns for explicit follow-ups.
	SubagentTurnTimeout time.Duration
	// SubagentLifetimeTurns and SubagentRunTurns are supervisor-provided
	// counters used to resume a worker without losing observability or its
	// current-run budget. They are internal worker flags, not user policy.
	SubagentLifetimeTurns int
	SubagentRunTurns      int

	// Subagent selects a named markdown profile for a subagent child.
	// It is intentionally an internal child flag; the parent subagent tool
	// passes only the profile name and the child discovers the definition
	// locally.
	Subagent string

	// AgentName/AgentDataDir/PermissionSet are populated by `zut run`
	// for local zutfile agents. They scope sessions and enforce the
	// manifest's declared file/bash permissions.
	AgentName     string
	AgentDataDir  string
	PermissionSet *tools.PermissionSet
}

// ParseArgs parses the process arguments (excluding argv[0]).
func ParseArgs(in []string) (Args, error) {
	a := Args{Mode: ModeInteractive, MaxSteps: 0, WithSkills: true}
	positional := []string{}
	webSearchPolicySet := false

	want := func(i *int, flag string) (string, error) {
		*i++
		if *i >= len(in) {
			return "", fmt.Errorf("%s requires a value", flag)
		}
		return in[*i], nil
	}

	for i := 0; i < len(in); i++ {
		arg := in[i]
		switch arg {
		case "--":
			// Everything after the terminator is prompt text, including
			// values that begin with a dash. This is used by subagent
			// workers and is also a conventional CLI escape hatch.
			positional = append(positional, in[i+1:]...)
			i = len(in)
		case "-h", "--help":
			a.Help = true
		case "-v", "--version":
			a.Version = true
		case "-p", "--print":
			a.Mode = ModePrint
		case "--stream":
			a.Mode = ModeStream
		case "--json":
			a.Mode = ModeJSON
		case "--orchestrate":
			a.Orchestrate = true
		case "--rpc":
			a.Mode = ModeRPC
		case "-c", "--continue":
			a.Continue = true
		case "-r", "--resume":
			a.Resume = true
			if i+1 < len(in) {
				if id, err := uuid.Parse(in[i+1]); err == nil {
					a.ResumeSessionID = id.String()
					i++
				}
			}
		case "--no-session":
			a.NoSess = true
		case "--no-tools":
			a.NoTools = true
		case "--no-lsp":
			a.NoLSP = true
		case "--list-models":
			a.ListModels = true
		case "--experimental-oauth":
			// deprecated: subscription login is always available.
			// accepted silently for backwards compatibility.
		case "--provider":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.Provider = v
		case "--model":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.Model = v
		case "--api-key":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.APIKey = v
		case "--base-url":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.BaseURL = v
		case "--system-prompt":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.SystemPrompt = v
		case "--append-system-prompt":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.AppendSystemPrompt = append(a.AppendSystemPrompt, v)
		case "--ext", "-e":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			// Repeatable; each value is a directory containing an
			// extension.json. Resolved to absolute later so paths like
			// "." survive a later cwd change.
			a.Exts = append(a.Exts, v)
		case "--no-ext", "--no-extensions":
			a.NoExt = true
		case "--no-skill", "--no-skills":
			a.NoSkill = true
		case "--with-skills", "--with-skill":
			// Deprecated no-op: user skills are loaded by default.
			a.WithSkills = true
		case "--insecure":
			a.InsecureTLS = true
		case "--no-yolo":
			a.NoYolo = true
		case "-y", "--yes":
			a.Yes = true
		case "--reasoning":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			switch strings.ToLower(v) {
			case "", "off", "minimum", "minimal", "low", "medium", "high", "xhigh", "maximum", "max":
				a.Reasoning = strings.ToLower(v)
			default:
				return a, fmt.Errorf("--reasoning must be off|minimum|low|medium|high|xhigh|max")
			}
		case "--temperature":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			f, err := strconv.ParseFloat(v, 32)
			if err != nil || f < 0 || f > 2 {
				return a, fmt.Errorf("--temperature must be a number between 0 and 2")
			}
			t := float32(f)
			a.Temperature = &t
		case "--fast-mode":
			a.FastMode = true
			a.FastModeSet = true
		case "--no-fast-mode":
			a.FastMode = false
			a.FastModeSet = true
		case "--stats":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.StatsPath = v
		case "--session":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.Session = v
		case "--cwd":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.CWD = v
		case "--subagent-worker":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.SubagentWorker = v
			a.Mode = ModeSubagentWorker
		case "--subagent":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.Subagent = v
		case "--web-search-policy":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			webSearchPolicySet = true
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "inherit":
				a.WebSearchPolicy = subagents.WebSearchInherit
			case "deny", "off":
				a.WebSearchPolicy = subagents.WebSearchDeny
			case "allow", "on":
				a.WebSearchPolicy = subagents.WebSearchAllow
			default:
				return a, fmt.Errorf("--web-search-policy must be inherit|allow|deny")
			}
		case "--tools":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.ToolsSet = true
			for _, t := range strings.Split(v, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					a.Tools = append(a.Tools, t)
				}
			}
		case "--max-steps":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
				return a, fmt.Errorf("--max-steps must be a positive integer")
			}
			a.MaxSteps = n
		case "--max-turns":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
				return a, fmt.Errorf("--max-turns must be a positive integer")
			}
			a.SubagentMaxTurns = n
		case "--subagent-turn-timeout":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			timeout, err := time.ParseDuration(v)
			if err != nil || timeout <= 0 {
				return a, fmt.Errorf("--subagent-turn-timeout must be a positive duration")
			}
			a.SubagentTurnTimeout = timeout
		case "--subagent-lifetime-turns":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 0 {
				return a, fmt.Errorf("--subagent-lifetime-turns must be a non-negative integer")
			}
			a.SubagentLifetimeTurns = n
		case "--subagent-run-turns":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 0 {
				return a, fmt.Errorf("--subagent-run-turns must be a non-negative integer")
			}
			a.SubagentRunTurns = n
		default:
			if strings.HasPrefix(arg, "-") && arg != "-" {
				return a, fmt.Errorf("unknown flag %q", arg)
			}
			positional = append(positional, arg)
		}
	}

	if len(positional) > 0 {
		a.Prompt = strings.Join(positional, " ")
	}

	if webSearchPolicySet && a.Mode != ModeSubagentWorker {
		return a, fmt.Errorf("--web-search-policy requires --subagent-worker")
	}
	if a.StatsPath != "" && a.Mode != ModePrint {
		return a, fmt.Errorf("--stats requires -p or --print")
	}
	if a.CWD == "" {
		a.CWD, _ = os.Getwd()
	}
	return a, nil
}

// PrintHelp writes the help text to stderr. When stderr is a TTY it
// uses the same palette as zut's TUI; when redirected it falls back to
// plain text with no ANSI escapes.
func PrintHelp(version string) {
	th := tui.Dark
	fd := int(os.Stderr.Fd())
	useColor := term.IsTerminal(fd)
	if useColor && tui.DetectTrueColor(os.Getenv("TERM"), os.Getenv("COLORTERM")) {
		th.Terminal.TrueColor = true
	}
	style := func(c tui.TerminalColor, s string) string {
		if !useColor {
			return s
		}
		return th.FGColor(c, s)
	}
	assistant := func(s string) string { return style(th.Assistant, s) }
	muted := func(s string) string { return style(th.Muted, s) }
	fg := func(s string) string { return style(th.FG, s) }
	width := 96
	if useColor {
		if w, _, err := term.GetSize(fd); err == nil && w > 20 {
			width = w
		}
	}
	ruleWidth := width
	if ruleWidth < 40 {
		ruleWidth = 40
	}
	rule := strings.Repeat("─", ruleWidth)
	if useColor {
		rule = muted(rule)
	}
	leftW := 34
	if width >= 120 {
		leftW = 40
	}
	if width >= 140 {
		leftW = 46
	}
	type row struct{ left, right string }
	section := func(title string, rows ...row) {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, assistant(title))
		fmt.Fprintln(os.Stderr, rule)
		narrow := width < 100
		for _, r := range rows {
			if narrow {
				fmt.Fprintf(os.Stderr, "  %s\n", fg(r.left))
				fmt.Fprintf(os.Stderr, "    %s\n", muted(r.right))
				fmt.Fprintln(os.Stderr)
				continue
			}
			left := r.left
			if len([]rune(left)) < leftW {
				left += strings.Repeat(" ", leftW-len([]rune(left)))
			}
			fmt.Fprintf(os.Stderr, "  %s    %s\n", fg(left), muted(r.right))
		}
	}

	fmt.Fprintln(os.Stderr)
	var headline string
	if useColor {
		headline = th.AccentBar(th.Assistant) + assistant(tui.Bold("zut. yet another coding agent harness."))
	} else {
		headline = "zut. yet another coding agent harness."
	}
	fmt.Fprintln(os.Stderr, headline)
	fmt.Fprintln(os.Stderr, muted("ask anything, or type /help inside the tui to see commands."))
	fmt.Fprintf(os.Stderr, "%s %s\n", muted("version:"), fg(version))

	section("modes",
		row{"zut", "interactive tui"},
		row{"zut \"prompt\"", "interactive, pre-filled prompt"},
		row{"zut -p \"prompt\"", "print final text, exit"},
		row{"zut -p --orchestrate \"prompt\"", "delegate independent work, print final synthesis, exit"},
		row{"zut --stream \"prompt\"", "stream assistant text live, exit"},
		row{"zut --json \"prompt\"", "newline-delimited json events, exit"},
		row{"zut rpc", "json-rpc loop on stdin/stdout (see docs/rpc.md)"},
	)
	section("extensions",
		row{"zut ext list", "list installed extensions"},
		row{"zut ext install <path|git-url>", "install from a local path or Git URL"},
		row{"zut ext install --build=go <path>", "build local Go source, then install"},
		row{"zut --ext ./path/to/ext", "load an extension for this run only"},
		row{"zut ext help", "show all extension subcommands"},
	)
	section("self-update",
		row{"zut update", "download and install the latest release"},
		row{"zut update --check", "show whether a new release is available"},
	)
	section("telegram",
		row{"zut telegram-bot setup", "configure a telegram bot (from BotFather)"},
		row{"zut telegram-bot run", "foreground bridge (ctrl+c to stop)"},
		row{"zut telegram-bot start", "background bridge (detached)"},
		row{"zut telegram-bot stop", "stop the background bridge"},
		row{"zut telegram-bot logs [-f]", "tail the background bridge log"},
		row{"zut telegram-bot status", "config + running state"},
		row{"zut telegram-bot reset", "forget saved token"},
		row{"zut tg ...", "short alias for telegram-bot"},
	)
	section("provider and model flags",
		row{"--provider", "provider to use (anthropic|openai|openai-codex|kimi|deepseek|google|ollama|llama.cpp)"},
		row{"--model ID", "model id (see --list-models)"},
		row{"--api-key KEY", "api key for this run (env / auth.json fallback)"},
		row{"--base-url URL", "override provider api base url"},
		row{"--insecure", "skip TLS certificate verification (for self-signed-cert endpoints)"},
		row{"--reasoning off|minimum|low|medium|high|xhigh|max", "set reasoning level on supported models"},
		row{"--temperature N", "sampling temperature, 0 to 2 (omit for provider default)"},
	)
	section("prompt and session flags",
		row{"--orchestrate", "headless delegation and final synthesis (print, stream, or json modes)"},
		row{"--stats PATH", "write print-mode generation stats as json"},
		row{"--system-prompt TEXT", "replace the default system prompt"},
		row{"--append-system-prompt TEXT", "append to the system prompt (repeatable)"},
		row{"-c, --continue", "continue the most recent session for this cwd"},
		row{"-r, --resume [UUID]", "pick a session, or resume the persisted UUID"},
		row{"--session PATH", "resume a specific session file"},
		row{"--no-session", "do not read or write a session file"},
	)
	section("workspace, tools, skills",
		row{"--cwd PATH", "treat PATH as the working directory"},
		row{"--no-tools", "disable all tools"},
		row{"--no-lsp", "disable the built-in LSP/linter tool"},
		row{"--tools csv", "only enable listed tools (include lsp and web_search explicitly)"},
		row{"--no-yolo", "ask before running every tool call"},
		row{"-y, --yes", "accept zut run consent without prompting"},
		row{"--no-ext", "skip extension discovery for this run"},
		row{"--no-skill", "skip all skill discovery for this run"},
	)
	section("misc",
		row{"--max-steps N", "agent loop iteration cap (default: unlimited)"},
		row{"--list-models", "print known models and exit"},
		row{"-h, --help", "show this help"},
		row{"-v, --version", "show version info"},
	)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, assistant("see also: docs/extensions.md, docs/rpc.md, docs/skills.md"))
	fmt.Fprintln(os.Stderr)
}
