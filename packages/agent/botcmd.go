package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bnema/zut/packages/agent/daemon"
	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/modes/telegram"
	"golang.org/x/term"
)

// runBotCommand dispatches `zut <protocol>-bot ...` subcommands via
// the botSpec registry. Returns true if rawArgs begins with a
// recognised subcommand, false otherwise. The short alias "tg" (and
// "mx" for matrix) are also accepted.
func runBotCommand(rawArgs []string, version string) (handled bool, err error) {
	if len(rawArgs) == 0 {
		return false, nil
	}
	spec := specFor(rawArgs[0])
	if spec == nil {
		return false, nil
	}
	sub := ""
	var tail []string
	if len(rawArgs) >= 2 {
		sub = rawArgs[1]
		tail = rawArgs[2:]
	}
	switch sub {
	case "", "help", "-h", "--help":
		spec.printHelp()
		return true, nil
	case "setup":
		return true, spec.setup(tail)
	case "status":
		return true, spec.status(os.Stdout, version)
	case "reset":
		return true, spec.reset()
	case "start":
		return true, botStart(spec, tail, version)
	case "stop":
		return true, botStop(spec, version)
	case "logs":
		return true, botLogs(spec, tail)
	default:
		spec.printHelp()
		return true, fmt.Errorf("unknown %s subcommand %q", spec.subcommand, sub)
	}
}

// botStart asks the daemon (spawned when absent) to run the bot service in
// one headless session created with this cwd, environment and flags.
func botStart(spec *botSpec, rawTail []string, version string) error {
	if ok, cerr := spec.configured(ZutHome()); cerr != nil {
		return cerr
	} else if !ok {
		return fmt.Errorf("%s is not configured — run `zut %s setup` first", spec.name, spec.subcommand)
	}
	botSpec, err := botStartSpec(spec.name, rawTail)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), botRequestTimeout)
	defer cancel()
	conn, err := dialBotDaemon(ctx, version, true)
	if err != nil {
		return err
	}
	defer conn.close()
	var st daemonproto.BotStatusData
	if err := conn.call(ctx, daemonproto.BotStartRequest{Type: "bot_start", Spec: botSpec}, &st); err != nil {
		return fmt.Errorf("start %s bot: %w", spec.name, err)
	}
	fmt.Printf("started zut %s in the daemon (session %s)\n", spec.subcommand, st.Session)
	fmt.Printf("use `zut %s logs -f` to tail, `zut %s stop` to stop.\n", spec.subcommand, spec.subcommand)
	return nil
}

// botStartSpec captures the client cwd, environment and session flags. The
// flags are validated here so typos fail before the daemon is involved.
func botStartSpec(kind string, rawTail []string) (daemonproto.BotSpec, error) {
	args, err := ParseArgs(rawTail)
	if err != nil {
		return daemonproto.BotSpec{}, err
	}
	cwd, err := filepath.Abs(args.CWD)
	if err != nil {
		return daemonproto.BotSpec{}, fmt.Errorf("resolve cwd: %w", err)
	}
	return daemonproto.BotSpec{Kind: kind, CWD: cwd, Env: os.Environ(), Args: append([]string(nil), rawTail...)}, nil
}

// botStop asks the daemon to stop the bot service and close its session.
func botStop(spec *botSpec, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), botRequestTimeout)
	defer cancel()
	conn, err := dialBotDaemon(ctx, version, false)
	if errors.Is(err, errDaemonAbsent) {
		fmt.Printf("%s is not running (zut daemon is not running)\n", spec.name)
		return nil
	}
	if err != nil {
		return err
	}
	defer conn.close()
	var before daemonproto.BotStatusData
	if err := conn.call(ctx, daemonproto.BotStatusRequest{Type: "bot_status", Kind: spec.name}, &before); err != nil {
		return fmt.Errorf("%s bot status: %w", spec.name, err)
	}
	if !before.Running {
		fmt.Printf("%s is not running\n", spec.name)
		return nil
	}
	if err := conn.call(ctx, daemonproto.BotStopRequest{Type: "bot_stop", Kind: spec.name}, nil); err != nil {
		return fmt.Errorf("stop %s bot: %w", spec.name, err)
	}
	fmt.Printf("stopped %s (session %s closed)\n", spec.name, before.Session)
	return nil
}

// botLogs prints the bot lines of the daemon log (with --follow, keeps
// printing new ones). The daemon log holds every bot diagnostic.
func botLogs(spec *botSpec, rawTail []string) error {
	follow := false
	for _, a := range rawTail {
		if a == "-f" || a == "--follow" {
			follow = true
		}
	}
	p := daemon.LogPath(ZutHome())
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("no log file at", p)
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	filter := &botLogFilter{w: os.Stdout, kind: spec.name}
	if _, err := io.Copy(filter, f); err != nil {
		return err
	}
	if !follow {
		return nil
	}

	// Naive tail -f: poll for new bytes until ctrl+c.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigc)
	for {
		select {
		case <-sigc:
			return nil
		case <-time.After(500 * time.Millisecond):
			if _, err := io.Copy(filter, f); err != nil {
				return err
			}
		}
	}
}

// maskToken returns "123456:ABC...xyz" so copies of zut telegram-bot status can be
// pasted into bug reports without leaking the full token.
func maskToken(tok string) string {
	if len(tok) <= 10 {
		return "<hidden>"
	}
	// telegram tokens look like "123456789:ABCD..." — keep the id, mask the body.
	i := strings.IndexByte(tok, ':')
	if i < 0 {
		return tok[:4] + "..." + tok[len(tok)-4:]
	}
	body := tok[i+1:]
	if len(body) < 8 {
		return tok[:i+1] + "<hidden>"
	}
	return tok[:i+1] + body[:3] + "..." + body[len(body)-3:]
}

// telegramSpec wires the existing Telegram setup/status/reset flows
// into the generic dispatcher. Bodies are the pre-refactor botSetup,
// botStatus, botReset, printBotHelp — unchanged.
func telegramSpec() *botSpec {
	return &botSpec{
		name:       "telegram",
		subcommand: "telegram-bot",
		aliases:    []string{"tg"},
		configured: func(zutHome string) (bool, error) {
			cfg, err := telegram.LoadConfig(zutHome)
			if err != nil {
				return false, err
			}
			return cfg.BotToken != "", nil
		},
		printHelp: printTelegramBotHelp,
		setup:     telegramBotSetup,
		status:    telegramBotStatus,
		reset:     telegramBotReset,
	}
}

// printTelegramBotHelp prints usage for `zut telegram-bot`.
func printTelegramBotHelp() {
	fmt.Fprint(os.Stderr, `zut telegram-bot — telegram bridge

usage:
  zut telegram-bot setup                       paste a BotFather token, verify, save
  zut telegram-bot status                      show bot config and whether the daemon runs it
  zut telegram-bot start [flags]               run the bot in the zut daemon (starts the daemon if needed)
  zut telegram-bot stop                        stop the bot and close its session
  zut telegram-bot logs [--follow]             print the bot lines of the daemon log
  zut telegram-bot reset                       forget token + allowed user

setup flow:
  1. talk to @BotFather on telegram, /newbot, copy the token
  2. run "zut telegram-bot setup" and paste the token
  3. run "zut telegram-bot start" in the directory the agent should work in
  4. send /start to your bot from telegram; the first sender claims it

the bot runs inside the zut daemon as one headless session (see zut ls).
start flags (--provider, --model, --reasoning, ...) configure that session.
dm the bot anything and the message is forwarded to the session. image
attachments (photos or image/* documents) are passed to vision-capable
models. telegram commands the bot handles directly: /help, /status, /stop.
the bot is not restarted automatically after a daemon restart; its session
is restored idle and the next "start" reuses it.

config & state:
  $ZUT_HOME/bot.json          # bot token + paired user (mode 0600)
  $ZUT_HOME/logs/daemon.log   # daemon log, including bot lines
`)
}

var (
	botTokenIsTerminal   = term.IsTerminal
	botTokenReadPassword = term.ReadPassword
)

func readBotToken(in *os.File, out io.Writer) (string, error) {
	if botTokenIsTerminal(int(in.Fd())) {
		value, err := botTokenReadPassword(int(in.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return "", fmt.Errorf("read telegram bot token: %w", err)
		}
		return string(value), nil
	}

	value, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read telegram bot token: %w", err)
	}
	return value, nil
}

// telegramBotSetup interactively reads a bot token without echoing it on a
// terminal, verifies it via getMe, and saves it.
func telegramBotSetup(_ []string) error {
	cfg, err := telegram.LoadConfig(ZutHome())
	if err != nil {
		return err
	}

	fmt.Print("telegram bot token (from @BotFather): ")
	line, err := readBotToken(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(line)
	if token == "" {
		return fmt.Errorf("no token provided")
	}

	client := telegram.NewClient(token)
	me, err := client.GetMe(context.Background())
	if err != nil {
		return fmt.Errorf("token rejected by telegram: %w", err)
	}
	cfg.BotToken = token
	cfg.BotUsername = me.Username
	cfg.BotID = me.ID
	// Any stored pairing might be for a different bot; clear it.
	cfg.AllowedUserID = 0
	cfg.LastUpdateID = 0
	if err := telegram.SaveConfig(ZutHome(), cfg); err != nil {
		return err
	}
	fmt.Printf("\nsaved: @%s (id=%d) to %s\n", me.Username, me.ID, telegram.ConfigPath(ZutHome()))
	fmt.Println("next: run `zut telegram-bot start`, then send /start to your bot from telegram.")
	return nil
}

// telegramBotStatus prints the bot config without the token, plus whether
// the daemon runs the bot service or a session bridge.
func telegramBotStatus(w io.Writer, version string) error {
	cfg, err := telegram.LoadConfig(ZutHome())
	if err != nil {
		return err
	}
	if cfg.BotToken == "" {
		fmt.Fprintln(w, "telegram: not configured (run `zut telegram-bot setup`)")
		return nil
	}
	fmt.Fprintf(w, "telegram bot: @%s (id=%d)\n", cfg.BotUsername, cfg.BotID)
	fmt.Fprintf(w, "token:        %s\n", maskToken(cfg.BotToken))
	if cfg.AllowedUserID == 0 {
		fmt.Fprintln(w, "paired with:  (unpaired — send /start from telegram to claim)")
	} else {
		fmt.Fprintf(w, "paired with:  telegram user id %d\n", cfg.AllowedUserID)
	}
	fmt.Fprintf(w, "last update:  %d\n", cfg.LastUpdateID)
	fmt.Fprintf(w, "config file:  %s\n", telegram.ConfigPath(ZutHome()))

	ctx, cancel := context.WithTimeout(context.Background(), botRequestTimeout)
	defer cancel()
	conn, err := dialBotDaemon(ctx, version, false)
	if errors.Is(err, errDaemonAbsent) {
		fmt.Fprintln(w, "service:      stopped (zut daemon is not running)")
		return nil
	}
	if err != nil {
		return err
	}
	defer conn.close()
	var st daemonproto.BotStatusData
	if err := conn.call(ctx, daemonproto.BotStatusRequest{Type: "bot_status", Kind: "telegram"}, &st); err != nil {
		return fmt.Errorf("telegram bot status: %w", err)
	}
	writeBotServiceStatus(w, st)
	return nil
}

func writeBotServiceStatus(w io.Writer, st daemonproto.BotStatusData) {
	switch {
	case st.Running:
		fmt.Fprintf(w, "service:      running in the zut daemon (session %s, since %s)\n", st.Session, st.StartedAt.Local().Format(time.DateTime))
	case st.LastError != "":
		fmt.Fprintf(w, "service:      stopped (last error: %s)\n", st.LastError)
	default:
		fmt.Fprintln(w, "service:      stopped")
	}
	if st.Bridge != nil && st.Bridge.Active {
		fmt.Fprintf(w, "bridge:       mirroring session %s\n", st.Bridge.Session)
	}
}

// telegramBotReset wipes the on-disk bot.json entry.
func telegramBotReset() error {
	p := telegram.ConfigPath(ZutHome())
	if _, err := os.Stat(p); os.IsNotExist(err) {
		fmt.Println("no bot config to reset")
		return nil
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	fmt.Println("removed", p)
	return nil
}
