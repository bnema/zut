package agent

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
)

func runDebugCommand(rawArgs []string, stdout io.Writer) (bool, error) {
	if len(rawArgs) == 0 || rawArgs[0] != "debug" {
		return false, nil
	}
	if len(rawArgs) != 3 || rawArgs[1] != "trace" {
		return true, fmt.Errorf("usage: zut debug trace <bundle>")
	}
	events, err := subagents.ReadTrace(rawArgs[2])
	if err != nil {
		return true, err
	}
	return true, renderTraceInspection(stdout, subagents.ProjectTrace(events), time.Now())
}

func renderTraceInspection(w io.Writer, views map[string]subagents.AgentTraceView, now time.Time) error {
	ids := make([]string, 0, len(views))
	for id := range views {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		view := views[id]
		if view.Terminal != "" {
			if _, err := fmt.Fprintf(w, "%s  %s\n", id, view.Terminal); err != nil {
				return err
			}
			continue
		}
		if len(view.OpenOperations) != 0 {
			operation := view.OpenOperations[0]
			if _, err := fmt.Fprintf(w, "%s  %s open %s\n", id, strings.Replace(operation.Type, ".started", "", 1), operation.Duration(now).Round(time.Second)); err != nil {
				return err
			}
			continue
		}
		age := now.Sub(view.LastEvent.Timestamp).Round(time.Second)
		when := age.String() + " ago"
		if age < 0 {
			when = "in " + (-age).String()
		}
		if _, err := fmt.Fprintf(w, "%s  no observable operation; last event %s %s\n", id, view.LastEvent.Type, when); err != nil {
			return err
		}
	}
	return nil
}
