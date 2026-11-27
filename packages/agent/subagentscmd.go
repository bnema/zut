package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/config"
	"github.com/bnema/zut/packages/agent/daemon"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/google/uuid"
)

const subagentsOfflineWarning = "Stop ALL zut processes using this ZUT_HOME (including standalone and older ones) and keep them stopped until prune finishes."

const subagentsHelp = `zut subagents - manage stored resident subagents

Usage: zut subagents prune [--older-than AGE] [--dry-run | -y | --yes]

  --older-than AGE  Select eligible children older than AGE (default: 7d).
                    Units: m, h, d, w, mo, y.
  --dry-run         Preview candidates without deleting anything.
  -y, --yes         Delete candidates without prompting.
  -h, --help        Show this help.

Destructive prune requires an offline ZUT_HOME.
` + subagentsOfflineWarning + `
Recently modified or unresolved parent sessions and protected children are preserved.
Preview candidates may be skipped after rechecking under the ownership lease.
Confirmation reads a full line from stdin; EOF declines. Scripts should use --yes.`

type subagentsPruneOptions struct {
	age         sessionAge
	dryRun, yes bool
}

func runSubagentsCommand(ctx context.Context, args []string) (bool, error) {
	if len(args) == 0 || args[0] != "subagents" {
		return false, nil
	}
	if len(args) == 1 || args[1] == "help" || args[1] == "-h" || args[1] == "--help" {
		if len(args) > 2 {
			return true, fmt.Errorf("unexpected argument after help: %s", args[2])
		}
		_, err := fmt.Fprintln(os.Stdout, subagentsHelp)
		return true, err
	}
	if args[1] != "prune" {
		return true, fmt.Errorf("unknown subagents subcommand: %s", args[1])
	}
	opts, err := parseSubagentsPruneOptions(args[2:])
	if errors.Is(err, errSubagentsPruneHelp) {
		_, err := fmt.Fprintln(os.Stdout, subagentsHelp)
		return true, err
	}
	if err != nil {
		return true, err
	}
	return true, runSubagentsPrune(ctx, config.ZutHome(), opts, os.Stdin, os.Stdout, os.Stderr)
}

var errSubagentsPruneHelp = errors.New("subagents prune help requested")

func parseSubagentsPruneOptions(args []string) (subagentsPruneOptions, error) {
	opts := subagentsPruneOptions{age: sessionAge{value: 7, unit: "d"}}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			if len(args) != 1 {
				return opts, errors.New("help cannot be combined with other prune arguments")
			}
			return opts, errSubagentsPruneHelp
		case "--dry-run":
			opts.dryRun = true
		case "-y", "--yes":
			opts.yes = true
		case "--older-than":
			i++
			if i == len(args) {
				return opts, errors.New("--older-than requires an age")
			}
			age, err := parseSessionAge(args[i])
			if err != nil {
				return opts, err
			}
			opts.age = age
		default:
			return opts, fmt.Errorf("unknown subagents prune flag: %s", args[i])
		}
	}
	if opts.yes && opts.dryRun {
		return opts, errors.New("--yes cannot be used with --dry-run")
	}
	return opts, nil
}

func runSubagentsPrune(ctx context.Context, home string, opts subagentsPruneOptions, in io.Reader, out, errOut io.Writer) (runErr error) {
	deleted := 0
	defer func() {
		if err := ctx.Err(); err != nil {
			runErr = errors.Join(runErr, reportPruneInterruption(out, deleted, err))
		}
	}()
	if ctx.Err() != nil {
		return nil
	}
	if !opts.dryRun {
		if opts.yes {
			if _, err := fmt.Fprintln(errOut, subagentsOfflineWarning); err != nil {
				return err
			}
		}
		lease, err := daemon.LockHome(home)
		if err != nil {
			return fmt.Errorf("destructive prune requires an offline ZUT_HOME; stop zut and its daemon first (cannot acquire daemon lock): %w", err)
		}
		defer lease.Close()
	}
	now := time.Now()
	cutoff, err := opts.age.cutoff(now)
	if err != nil {
		return err
	}
	type parentFile struct {
		path string
		info os.FileInfo
	}
	paths := make(map[string]parentFile)
	unresolved := 0
	defer func() {
		if unresolved != 0 {
			_, err := fmt.Fprintf(errOut, "warning: preserved %d child check(s) with unresolved parent sessions\n", unresolved)
			runErr = errors.Join(runErr, err)
		}
	}()
	parentActivity := func(id string, fresh bool) (time.Time, error) {
		if suffix, ok := strings.CutPrefix(id, residentRuntimeScopePrefix); ok {
			if parsed, err := uuid.Parse(suffix); err == nil && parsed.String() == suffix {
				return time.Time{}, nil
			}
		}
		unverified := func() (time.Time, error) {
			unresolved++
			return time.Time{}, subagents.ErrPruneParentUnresolved
		}
		previous, known := paths[id]
		path := previous.path
		if !known || fresh {
			var err error
			path, err = core.FindManagedSessionByID(ctx, home, id)
			if err != nil || path == "" || (known && path != previous.path) {
				return unverified()
			}
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || (known && !os.SameFile(previous.info, info)) {
			return unverified()
		}
		// Verify the opened file's header too: an in-place rewrite can keep
		// the same inode, and lookup and stat are separate operations.
		if !known || fresh {
			if err := core.VerifySessionHeaderID(path, info, id); err != nil {
				return unverified()
			}
			paths[id] = parentFile{path, info}
		}
		return info.ModTime(), nil
	}
	pruner := subagents.ResidentPruner{
		Root: filepath.Join(home, "subagents"), Cutoff: cutoff,
		ParentActivity:        func(id string) (time.Time, error) { return parentActivity(id, false) },
		RecheckParentActivity: func(id string) (time.Time, error) { return parentActivity(id, true) },
	}
	candidates, notices, err := pruner.Scan()
	for _, notice := range notices {
		if _, writeErr := fmt.Fprintf(errOut, "warning: %v\n", notice); writeErr != nil {
			return writeErr
		}
	}
	if err != nil {
		return err
	}
	var bytes int64
	for _, c := range candidates {
		if _, err := fmt.Fprintf(out, "  %.8s  %s  profile=%q  age=%s  %d bytes\n", c.ID, c.State, c.Profile, now.Sub(c.UpdatedAt).Truncate(time.Minute), c.SizeBytes); err != nil {
			return err
		}
		bytes += c.SizeBytes
	}
	if _, err := fmt.Fprintf(out, "%d candidate(s), %d bytes\n", len(candidates), bytes); err != nil {
		return err
	}
	if opts.dryRun {
		_, err := fmt.Fprintln(out, "dry run: nothing deleted")
		return err
	}
	if len(candidates) == 0 {
		return nil
	}
	if !opts.yes {
		if _, err := fmt.Fprintln(errOut, subagentsOfflineWarning); err != nil {
			return err
		}
		confirmed, err := confirmSubagentsPrune(ctx, in, errOut, len(candidates))
		if err != nil {
			return err
		}
		if !confirmed {
			_, err := fmt.Fprintln(out, "no resident children deleted")
			return err
		}
	}
	deleted, err = deletePruneCandidates(ctx, pruner, candidates, errOut)
	if ctx.Err() != nil {
		return err
	}
	_, writeErr := fmt.Fprintf(out, "deleted %d resident child(ren)\n", deleted)
	return errors.Join(err, writeErr)
}

func reportPruneInterruption(out io.Writer, deleted int, cause error) error {
	_, err := fmt.Fprintf(out, "deleted %d resident child(ren) before interruption\n", deleted)
	return errors.Join(fmt.Errorf("subagents prune interrupted: %w", cause), err)
}

func deletePruneCandidates(ctx context.Context, pruner subagents.ResidentPruner, candidates []subagents.ResidentPruneCandidate, errOut io.Writer) (int, error) {
	var failures []error
	deleted := 0
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			break
		}
		if err := pruner.Delete(ctx, c); err != nil {
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				break
			}
			if errors.Is(err, subagents.ErrPruneParentUnresolved) {
				continue
			}
			if _, writeErr := fmt.Fprintf(errOut, "skipped %s: %v\n", c.ID, err); writeErr != nil {
				failures = append(failures, writeErr)
				break
			}
			if !errors.Is(err, subagents.ErrResidentLeaseBusy) {
				failures = append(failures, fmt.Errorf("prune %s: %w", c.ID, err))
			}
			continue
		}
		deleted++
	}
	return deleted, errors.Join(failures...)
}

func confirmSubagentsPrune(ctx context.Context, in io.Reader, out io.Writer, count int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := fmt.Fprintf(out, "permanently delete %d resident child(ren)? [y/N]: ", count); err != nil {
		return false, err
	}
	type answer struct {
		confirmed bool
		err       error
	}
	done := make(chan answer, 1)
	// The caller owns stdin. A blocked read must not keep the command alive;
	// the buffered channel also lets it finish if input arrives after cancellation.
	go func() { confirmed, err := readPruneAnswer(bufio.NewReader(in), true); done <- answer{confirmed, err} }()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case a := <-done:
		return a.confirmed, a.err
	}
}
