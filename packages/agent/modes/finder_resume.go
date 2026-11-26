package modes

import (
	"path/filepath"
	"strings"

	"github.com/mattn/go-runewidth"

	"github.com/bnema/zut/packages/tui"
)

// resumePlan describes a session the user picked in the finder.
type resumePlan struct {
	Path       string // session file
	LiveID     string // daemon session id when Live
	TargetCWD  string // directory the session ran in
	CurrentCWD string // directory of this terminal
	CWDExists  bool   // TargetCWD still exists
	Live       bool   // session is running in the daemon
}

type resumeChoice int

const (
	resumeChoiceNone   resumeChoice = iota
	resumeChoiceResume              // same directory: resume without asking
	resumeChoiceSwitch              // switch to TargetCWD and resume
	resumeChoiceFork                // fork into CurrentCWD
	resumeChoiceAttach              // attach to the live session
	resumeChoiceCancel
)

// resumeConfirm asks how to open a session from another directory.
type resumeConfirm struct {
	plan resumePlan
}

// decideResume returns nil when the session can be resumed directly
// (same directory), otherwise the confirmation to show.
func decideResume(plan resumePlan) *resumeConfirm {
	if plan.TargetCWD == "" || filepath.Clean(plan.TargetCWD) == filepath.Clean(plan.CurrentCWD) {
		return nil
	}
	return &resumeConfirm{plan: plan}
}

func (c *resumeConfirm) Active() bool { return c != nil }

func (c *resumeConfirm) canSwitch() bool { return !c.plan.Live && c.plan.CWDExists }
func (c *resumeConfirm) canFork() bool   { return !c.plan.Live }

// HandleKey maps a key to a choice; keys that are not offered for this
// plan return resumeChoiceNone.
func (c *resumeConfirm) HandleKey(k tui.Key) resumeChoice {
	switch k.Kind {
	case tui.KeyEsc, tui.KeyCtrlC:
		return resumeChoiceCancel
	case tui.KeyRune:
		if k.Ctrl || k.Alt {
			return resumeChoiceNone
		}
		switch k.Rune {
		case 'y', 'Y':
			if c.plan.Live {
				return resumeChoiceAttach
			}
			if c.canSwitch() {
				return resumeChoiceSwitch
			}
		case 'n', 'N':
			if c.canFork() {
				return resumeChoiceFork
			}
		}
	}
	return resumeChoiceNone
}

// resumeConfirmLine is one logical line: a lead (a key drawn in the
// accent color, or a label) followed by text whose continuation rows are
// indented under the text.
type resumeConfirmLine struct {
	lead, text string
	key, warn  bool
}

func (c *resumeConfirm) lines() []resumeConfirmLine {
	target := sanitizeSessionTreeText(friendlyPath(c.plan.TargetCWD))
	current := sanitizeSessionTreeText(friendlyPath(c.plan.CurrentCWD))
	if c.plan.Live {
		return []resumeConfirmLine{
			{lead: "This session is running in ", text: target},
			{lead: "[y]", key: true, text: "attach (runs in " + target + ")"},
			{lead: "esc", key: true, text: "cancel"},
		}
	}
	out := []resumeConfirmLine{
		{lead: "This session ran in ", text: target},
		{lead: "You are in          ", text: current},
	}
	if !c.plan.CWDExists {
		out = append(out, resumeConfirmLine{text: "original directory no longer exists", warn: true})
	} else {
		out = append(out, resumeConfirmLine{lead: "[y]", key: true, text: "switch to " + target + " and resume"})
	}
	return append(out,
		resumeConfirmLine{lead: "[n]", key: true, text: "fork into " + current + " (new session, original stays untouched)"},
		resumeConfirmLine{lead: "esc", key: true, text: "cancel"},
	)
}

// Render draws the confirmation box; every line is exactly width cells.
func (c *resumeConfirm) Render(th tui.Theme, width int) []string {
	if width < 8 {
		return nil
	}
	inner := width - 2
	text := inner - 2 // one space of padding on each side
	border := func(s string) string { return th.FGColor(th.Muted, s) }
	out := []string{border(finderFitBorder("╭─ Resume session ", "╮", width))}
	row := func(s string) {
		out = append(out, border("│")+" "+s+" "+border("│"))
	}
	for _, line := range c.lines() {
		lead := line.lead
		if line.key {
			lead = " " + lead + " "
		}
		leadW := runewidth.StringWidth(lead)
		styledLead := lead
		if line.key {
			styledLead = " " + th.FGColor(th.Accent, line.lead) + " "
		}
		if leadW > text/2 {
			// Too narrow to indent under the lead: give it its own rows.
			for _, part := range wrapPlain(lead, text) {
				row(finderFit(part, text))
			}
			lead, styledLead, leadW = "", "", 0
		}
		for idx, part := range wrapPlain(line.text, text-leadW) {
			prefix := strings.Repeat(" ", leadW)
			if idx == 0 {
				prefix = styledLead
			}
			body := finderFit(part, text-leadW)
			if line.warn {
				body = th.FGColor(th.Warning, body)
			}
			row(prefix + body)
		}
	}
	out = append(out, border("╰"+strings.Repeat("─", inner)+"╯"))
	return out
}

// wrapPlain wraps plain text to w cells, breaking at spaces and hard-
// breaking words wider than w.
func wrapPlain(s string, w int) []string {
	if w <= 0 {
		return []string{""}
	}
	var rows []string
	var cur strings.Builder
	curW := 0
	flush := func() {
		rows = append(rows, cur.String())
		cur.Reset()
		curW = 0
	}
	for _, word := range strings.Fields(s) {
		ww := runewidth.StringWidth(word)
		if curW > 0 && curW+1+ww > w {
			flush()
		}
		if curW > 0 {
			cur.WriteByte(' ')
			curW++
		}
		for _, r := range word {
			rw := runewidth.RuneWidth(r)
			if curW+rw > w && curW > 0 {
				flush()
			}
			cur.WriteRune(r)
			curW += rw
		}
	}
	if cur.Len() > 0 || len(rows) == 0 {
		flush()
	}
	return rows
}
