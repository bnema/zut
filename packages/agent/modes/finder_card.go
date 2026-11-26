package modes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/bnema/zut/packages/agent/internal/sessionindex"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// finderCard is the preview of an item without hits: short facts and
// labeled excerpts.
type finderCard struct {
	Facts    []string
	Sections []finderCardSection
}

type finderCardSection struct {
	Label string
	Role  provider.Role
	Time  time.Time
	Text  string // sanitized, whitespace collapsed, capped
}

// finderCardTextRunes caps a section's text: far more than a preview pane
// shows, small enough to rewrap on every render.
const finderCardTextRunes = 4000

func finderCardText(s string) string {
	s = strings.Join(strings.Fields(sanitizeSessionTreeText(s)), " ")
	n := 0
	for i := range s {
		if n == finderCardTextRunes {
			return s[:i]
		}
		n++
	}
	return s
}

func (c finderCard) empty() bool { return len(c.Facts) == 0 && len(c.Sections) == 0 }

// finderCardState tracks the in-flight card fetch. Guarded by
// Interactive.mu.
type finderCardState struct {
	key    string
	gen    uint64
	cancel context.CancelFunc
}

func (s *finderCardState) reset() {
	if s.cancel != nil {
		s.cancel()
	}
	*s = finderCardState{}
}

// requestFinderCard loads the card of the selected item when it has no
// hits and no card yet. The fetch runs on a goroutine; moving to another
// item cancels it.
func (i *Interactive) requestFinderCard(ctx context.Context) {
	i.mu.Lock()
	defer i.mu.Unlock()
	m, index := i.finder, i.finderIndex
	if index == nil || !m.Active() {
		return
	}
	item, ok := m.selected()
	gen := m.CardsGeneration()
	if !ok || len(item.Hits) > 0 || item.Key == "" || m.hasFreshCard(item.Key) || (i.finderCard.key == item.Key && i.finderCard.gen == gen) {
		return
	}
	i.finderCard.reset()
	fetchCtx, cancel := context.WithCancel(ctx)
	i.finderCard = finderCardState{key: item.Key, gen: gen, cancel: cancel}
	key := item.Key
	go func() {
		defer cancel()
		ov, found, err := index.Overview(fetchCtx, key)
		if fetchCtx.Err() != nil {
			return // superseded or closed; the newer request owns the state
		}
		apply := func() {
			i.mu.Lock()
			defer i.mu.Unlock()
			if i.finder != m {
				return // finder closed or reopened
			}
			if i.finderCard.key == key && i.finderCard.gen == gen {
				i.finderCard = finderCardState{} // a failed fetch can be retried
			}
			if err != nil {
				return
			}
			card := finderCard{}
			if found {
				card = sessionCard(ov)
			}
			if m.SetCard(gen, key, card) {
				i.invalidate()
			}
		}
		select {
		case i.loopCalls <- apply:
		case <-fetchCtx.Done():
		}
	}()
}

// sessionCard turns an index overview into a finder card.
func sessionCard(ov sessionindex.Overview) finderCard {
	var c finderCard
	if ov.Doc.CWD != "" {
		c.Facts = append(c.Facts, friendlyPath(ov.Doc.CWD))
	}
	const day = "2006-01-02 15:04"
	switch {
	case !ov.Doc.Started.IsZero():
		c.Facts = append(c.Facts, "started "+ov.Doc.Started.Local().Format(day)+" · last "+ov.Doc.Updated.Local().Format(day))
	case !ov.Doc.Updated.IsZero():
		c.Facts = append(c.Facts, "last "+ov.Doc.Updated.Local().Format(day))
	}
	c.Facts = append(c.Facts, fmt.Sprintf("%d messages · you %d · agent %d", ov.UserMsgs+ov.AgentMsgs, ov.UserMsgs, ov.AgentMsgs))
	var model []string
	if m := strings.Trim(ov.Doc.Provider+"/"+ov.Doc.Model, "/"); m != "" {
		model = append(model, m)
	}
	if ov.Doc.Cost > 0 {
		model = append(model, fmt.Sprintf("$%.2f", ov.Doc.Cost))
	}
	if len(model) > 0 {
		c.Facts = append(c.Facts, strings.Join(model, " · "))
	}
	if ov.First != nil {
		c.Sections = append(c.Sections, finderCardSection{Label: "first request", Role: provider.RoleUser, Time: ov.First.Ref.Time, Text: finderCardText(ov.First.Text)})
	}
	if ov.Last != nil {
		c.Sections = append(c.Sections, finderCardSection{Label: "last reply", Role: provider.RoleAssistant, Time: ov.Last.Ref.Time, Text: finderCardText(ov.Last.Text)})
	}
	return c
}

// renderFinderCard renders item's card in exactly rows lines of w cells:
// the label (wrapped, up to 3 lines), dimmed facts, then each section's
// header and wrapped text sharing the remaining rows.
func renderFinderCard(th tui.Theme, item finderItem, card finderCard, w, rows int) []string {
	blank := strings.Repeat(" ", max(w, 0))
	dim := th.DimColor(th.Muted, 40)
	var out []string
	label := tui.WrapANSILine(" "+sanitizeSessionTreeText(item.Label), max(w, 1))
	for n, line := range label[:min(len(label), 3)] {
		if n == 0 {
			out = append(out, renderPickerLabel(th, line, w))
		} else {
			out = append(out, pickerFit(line, w))
		}
	}
	for _, fact := range card.Facts {
		out = append(out, th.FGColor(dim, pickerFit(" "+sanitizeSessionTreeText(fact), w)))
	}
	if n := len(card.Sections); n > 0 {
		room := rows - len(out) - 2*n // a blank line and a header per section
		for idx, sec := range card.Sections {
			budget := max(room/(n-idx), 1)
			lines := tui.WrapANSILine(sec.Text, max(w-1, 1))
			if len(lines) > budget {
				lines = lines[:budget]
				lines[budget-1] = runewidth.Truncate(lines[budget-1]+" …", max(w-1, 1), "…")
			}
			room -= len(lines)
			out = append(out, blank, renderRoleHeader(th, sec.Role, sec.Label, " · "+formatHitTime(sec.Time), w))
			for _, line := range lines {
				out = append(out, th.FGColor(th.FG, pickerFit(" "+line, w)))
			}
		}
	}
	if len(out) > rows {
		out = out[:rows]
	}
	for len(out) < rows {
		out = append(out, blank)
	}
	return out
}
