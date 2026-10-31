package agent

import (
	"errors"
	"fmt"

	"github.com/bnema/zut/packages/agent/subagents"
)

// residentReconcileNotice folds startup reconcile errors into one short line.
// Corrupt journals are quarantined by the subagents package and never report
// again, so they get a count only; other failures keep the first cause.
func residentReconcileNotice(errs []error) string {
	var quarantined, failed int
	var first error
	for _, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, subagents.ErrCorruptResidentJournal):
			quarantined++
		default:
			failed++
			if first == nil {
				first = err
			}
		}
	}
	var text string
	if quarantined > 0 {
		text = fmt.Sprintf("%d corrupt subagent %s quarantined (quarantined.json)", quarantined, plural(quarantined, "journal", "journals"))
	}
	if failed > 0 {
		if text != "" {
			text += "; "
		}
		text += fmt.Sprintf("%d subagent %s not restored: %v", failed, plural(failed, "child", "children"), first)
	}
	return text
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
