package agent

import (
	"errors"
	"fmt"

	"github.com/bnema/zut/packages/agent/subagents"
)

// residentReconcileNotice folds startup reconcile errors into one short line.
// Successfully quarantined journals never report again, so they get a count
// only; other failures, including a failed quarantine, keep the first cause.
func residentReconcileNotice(errs []error) string {
	var quarantined, failed int
	var first error
	for _, err := range errs {
		var q *subagents.ResidentQuarantineError
		switch {
		case err == nil:
		case errors.As(err, &q):
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
		text = fmt.Sprintf("%d corrupt subagent %s quarantined (quarantined.json)", quarantined, pluralNoun(quarantined, "journal", "journals"))
	}
	if failed > 0 {
		if text != "" {
			text += "; "
		}
		text += fmt.Sprintf("%d subagent %s not restored: %v", failed, pluralNoun(failed, "child", "children"), first)
	}
	return text
}

func pluralNoun(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
