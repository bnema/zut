package agent

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bnema/zut/packages/agent/daemonclient"
	"github.com/bnema/zut/packages/agent/session"
)

const daemonNotRunningMsg = "zut daemon is not running"

var errDaemonNotRunning = errors.New(daemonNotRunningMsg)

// dialExistingDaemon connects to a running daemon and never spawns one. The
// entry points carry no version, so the skew warning is not emitted.
func dialExistingDaemon(ctx context.Context) (*daemonclient.Client, error) {
	c, err := daemonclient.Dial(ctx, daemonclient.SocketPath(os.Getenv), daemonclient.Options{ClientKind: "cli"})
	if err != nil {
		if daemonAbsent(err) {
			return nil, errDaemonNotRunning
		}
		return nil, fmt.Errorf("zut daemon not reachable: %w", err)
	}
	return c, nil
}

// withDaemon runs fn against the running daemon and maps errors to an exit code.
func withDaemon(stderr io.Writer, fn func(ctx context.Context, c *daemonclient.Client) error) int {
	ctx, cancel := context.WithTimeout(context.Background(), daemonClientDeadline)
	defer cancel()
	c, err := dialExistingDaemon(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer c.Disconnect()
	if err := fn(ctx, c); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func parseJSONFlag(name string, args []string, stderr io.Writer) (jsonOut bool, ok bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	j := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return false, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected arguments %v\n", name, fs.Args())
		return false, false
	}
	return *j, true
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// runLsCommand handles `zut ls [--json]`.
func runLsCommand(args []string, stdout, stderr io.Writer) int {
	jsonOut, ok := parseJSONFlag("zut ls", args, stderr)
	if !ok {
		return 2
	}
	return withDaemon(stderr, func(ctx context.Context, c *daemonclient.Client) error {
		list, err := c.List(ctx)
		if err != nil {
			return fmt.Errorf("zut ls: %w", err)
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
		if jsonOut {
			if list == nil {
				list = []session.Info{}
			}
			return writeJSON(stdout, list)
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tATTACH\tSTATE\tACTIVITY\tCWD\tTITLE\tMODEL\tAGE")
		now := time.Now()
		for _, in := range list {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", in.ID, dash(string(in.Attach)), dash(string(in.Run)),
				dash(activityLabel(in.Activity)), dash(in.CWD), dash(in.Title), dash(in.Model), liveAge(now, in.CreatedAt))
		}
		return tw.Flush()
	})
}

// runTreeCommand handles `zut tree [--json]`.
func runTreeCommand(args []string, stdout, stderr io.Writer) int {
	jsonOut, ok := parseJSONFlag("zut tree", args, stderr)
	if !ok {
		return 2
	}
	return withDaemon(stderr, func(ctx context.Context, c *daemonclient.Client) error {
		tree, err := c.Tree(ctx)
		if err != nil {
			return fmt.Errorf("zut tree: %w", err)
		}
		if jsonOut {
			return writeJSON(stdout, tree)
		}
		for _, ws := range tree.Workspaces {
			fmt.Fprintln(stdout, dash(ws.RepoRoot))
			for _, s := range ws.Sessions {
				line := fmt.Sprintf("  %s  %s  %s", s.Info.ID, dash(string(s.Info.Attach)), dash(string(s.Info.Run)))
				if s.Info.Title != "" {
					line += "  " + s.Info.Title
				}
				fmt.Fprintln(stdout, line)
				for _, r := range s.Residents {
					rl := fmt.Sprintf("    %s  %s  %s", r.ID, dash(r.Profile), dash(r.State))
					if r.Task != "" {
						rl += "  " + r.Task
					}
					fmt.Fprintln(stdout, rl)
				}
			}
		}
		return nil
	})
}

// runKillCommand handles `zut kill <id>...`. Each argument is an exact ID or
// a unique ID prefix; every argument is attempted and any failure exits 1.
func runKillCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: zut kill <id>...")
		return 2
	}
	return withDaemon(stderr, func(ctx context.Context, c *daemonclient.Client) error {
		list, err := c.List(ctx)
		if err != nil {
			return fmt.Errorf("zut kill: %w", err)
		}
		var failed bool
		for _, arg := range args {
			id, err := resolveSessionID(list, arg)
			if err == nil {
				err = c.Kill(ctx, session.ID(id))
			}
			if err != nil {
				fmt.Fprintf(stderr, "zut kill: %v\n", err)
				failed = true
				continue
			}
			fmt.Fprintf(stdout, "killed %s\n", id)
		}
		if failed {
			return errors.New("zut kill: some sessions were not killed")
		}
		return nil
	})
}

// resolveSessionID matches an exact ID, then a unique ID prefix, like the
// sessions tool (titles are not accepted for kill).
func resolveSessionID(list []session.Info, arg string) (string, error) {
	if arg == "" {
		return "", errors.New("empty session ID")
	}
	var matches []string
	for _, in := range list {
		if in.ID == arg {
			return in.ID, nil
		}
		if strings.HasPrefix(in.ID, arg) {
			matches = append(matches, in.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no open session matches %q", arg)
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)
		return "", fmt.Errorf("session prefix %q is ambiguous: %s", arg, strings.Join(matches, ", "))
	}
}

func activityLabel(a session.Activity) string {
	if a.Tool != "" {
		if a.Kind == "" {
			return a.Tool
		}
		return a.Kind + ":" + a.Tool
	}
	return a.Kind
}

func liveAge(now, created time.Time) string {
	if created.IsZero() {
		return "-"
	}
	d := now.Sub(created)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
