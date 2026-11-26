package modes

import (
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/agent/internal/sessionindex"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// chatSearch is the vim-like in-chat search left active after resuming a
// session from a finder hit. Guarded by Interactive.mu.
type chatSearch struct {
	terms   []string
	matches []int // view message indexes, ascending
	cur     int   // position in matches
	active  bool
	hint    string // statusOK text owned by the search
}

// chatSearchMessageText returns the searchable text of a user or assistant
// message, or "" for other roles.
func chatSearchMessageText(m provider.Message) string {
	if m.Role != provider.RoleUser && m.Role != provider.RoleAssistant {
		return ""
	}
	var b strings.Builder
	for _, c := range m.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			b.WriteString(tb.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// chatSearchMatches reports whether every term is a prefix of some index
// token of text, mirroring the session index semantics.
func chatSearchMatches(text string, terms []string) bool {
	if text == "" || len(terms) == 0 {
		return false
	}
	found := make([]bool, len(terms))
	left := len(terms)
	sessionindex.Tokenize(text, func(tok string) {
		for k, t := range terms {
			if !found[k] && strings.HasPrefix(tok, t) {
				found[k] = true
				left--
			}
		}
	})
	return left == 0
}

// normalizeChatSearchTerms lowercases and splits terms the way the index
// tokenizer does, dropping duplicates.
func normalizeChatSearchTerms(terms []string) []string {
	toks, _ := sessionindex.ParseQuery(strings.Join(terms, " "))
	return toks
}

// startChatSearch activates in-chat search for terms over the current
// transcript and scrolls to the match at (or nearest after) focusMsgIdx.
// With no match the search stays inactive and a status error is shown.
func (i *Interactive) startChatSearch(terms []string, focusMsgIdx int) {
	terms = normalizeChatSearchTerms(terms)
	i.mu.Lock()
	var matches []int
	if len(terms) > 0 {
		for idx, m := range i.view.Messages {
			if chatSearchMatches(chatSearchMessageText(m), terms) {
				matches = append(matches, idx)
			}
		}
	}
	if len(matches) == 0 {
		i.chatSearch = chatSearch{}
		i.statusErr, i.statusOK = fmt.Sprintf("search %q: no match in this session", strings.Join(terms, " ")), ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	cur := len(matches) - 1
	for k, idx := range matches {
		if idx >= focusMsgIdx {
			cur = k
			break
		}
	}
	i.chatSearch = chatSearch{terms: terms, matches: matches, cur: cur, active: true}
	i.mu.Unlock()
	i.chatSearchJump()
}

// chatSearchNext moves to the next matching message, wrapping around.
func (i *Interactive) chatSearchNext() { i.chatSearchStep(1) }

// chatSearchPrev moves to the previous matching message, wrapping around.
func (i *Interactive) chatSearchPrev() { i.chatSearchStep(-1) }

func (i *Interactive) chatSearchStep(delta int) {
	i.mu.Lock()
	s := &i.chatSearch
	if !s.active || len(s.matches) == 0 {
		i.mu.Unlock()
		return
	}
	n := len(s.matches)
	s.cur = ((s.cur+delta)%n + n) % n
	i.mu.Unlock()
	i.chatSearchJump()
}

// chatSearchJump scrolls to the current match and refreshes the hint.
func (i *Interactive) chatSearchJump() {
	i.mu.Lock()
	if !i.chatSearch.active || len(i.chatSearch.matches) == 0 {
		i.mu.Unlock()
		return
	}
	msgIdx := i.chatSearch.matches[i.chatSearch.cur]
	turn := 0
	for k := 0; k <= msgIdx && k < len(i.view.Messages); k++ {
		if i.view.Messages[k].Role == provider.RoleUser {
			turn++
		}
	}
	i.mu.Unlock()
	i.applyJumpSelection(msgIdx, turn)
	i.mu.Lock()
	s := &i.chatSearch
	if s.active {
		s.hint = fmt.Sprintf("search %q %d/%d · ^n/^p · esc clear", strings.Join(s.terms, " "), s.cur+1, len(s.matches))
		if i.statusErr == "" {
			i.statusOK = s.hint
		}
	}
	i.mu.Unlock()
	i.invalidate()
}

// clearChatSearch deactivates the search and drops its status hint.
func (i *Interactive) clearChatSearch() {
	i.mu.Lock()
	changed := i.clearChatSearchLocked()
	i.mu.Unlock()
	if changed {
		i.invalidate()
	}
}

func (i *Interactive) clearChatSearchLocked() bool {
	if !i.chatSearch.active {
		return false
	}
	if i.statusOK != "" && i.statusOK == i.chatSearch.hint {
		i.statusOK = ""
	}
	i.chatSearch = chatSearch{}
	return true
}

// chatSearchActive reports whether an in-chat search is active.
func (i *Interactive) chatSearchActive() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.chatSearch.active
}

// handleChatSearchKey consumes ctrl+n, ctrl+p and a bare Esc while the
// search is active. Callers run it after modal dialogs had their turn.
func (i *Interactive) handleChatSearchKey(k tui.Key) bool {
	if !i.chatSearchActive() {
		return false
	}
	switch {
	case k.Kind == tui.KeyCtrlN:
		i.chatSearchNext()
		return true
	case k.Kind == tui.KeyCtrlP:
		i.chatSearchPrev()
		return true
	case isUnmodifiedEscape(k):
		// Slash/file popups and transient help/notes keep Esc priority.
		if i.suggest.Active(i.ed.Value()) || i.fileSuggest.Active(i.ed.Value()) {
			return false
		}
		i.mu.Lock()
		overlay := len(i.helpBlock) > 0 || len(i.extNotes) > 0
		i.mu.Unlock()
		if overlay {
			return false
		}
		i.clearChatSearch()
		return true
	}
	return false
}

// shiftAnchorsForInfoBlocks returns anchors re-based onto the stable rows
// after insertSessionInfoBlocks added its blocks.
func shiftAnchorsForInfoBlocks(anchors []tui.MessageAnchor, blocks []sessionInfoBlock, rowCount int) []tui.MessageAnchor {
	out := append([]tui.MessageAnchor(nil), anchors...)
	if len(blocks) == 0 {
		return out
	}
	// Mirror insertSessionInfoBlocks: each block is inserted at the row of
	// anchors[afterMessage] (or the end), never before an earlier insert.
	rowStart := 0
	for _, block := range blocks {
		insertAt := rowCount
		if block.afterMessage >= 0 && block.afterMessage < len(anchors) {
			insertAt = anchors[block.afterMessage].Row
		}
		if insertAt < rowStart {
			insertAt = rowStart
		}
		for k := range out {
			if anchors[k].Row >= insertAt {
				out[k].Row += len(block.lines)
			}
		}
		rowStart = insertAt
	}
	return out
}

// applyChatSearchLocked returns visible with matched terms highlighted on
// rows of matching messages and the current match tinted. visible[k] is
// chat row start+k; base is the number of rows above the stable transcript.
// visible is never mutated.
func (i *Interactive) applyChatSearchLocked(visible []string, start, base, stableLen int) []string {
	s := &i.chatSearch
	if !s.active || len(visible) == 0 || len(i.stableChatAnchors) == 0 {
		return visible
	}
	anchors := i.stableChatAnchors
	matched := make(map[int]bool, len(s.matches))
	for _, idx := range s.matches {
		matched[idx] = true
	}
	curIdx := s.matches[s.cur]
	out := append([]string(nil), visible...)
	end := start + len(visible)
	for k, a := range anchors {
		if !matched[a.MessageIdx] {
			continue
		}
		rowEnd := stableLen
		if k+1 < len(anchors) {
			rowEnd = anchors[k+1].Row
		}
		lo, hi := base+a.Row, base+rowEnd
		if lo < start {
			lo = start
		}
		if hi > end {
			hi = end
		}
		for r := lo; r < hi; r++ {
			line, _ := tui.HighlightTerms(out[r-start], s.terms, i.cfg.Theme)
			if a.MessageIdx == curIdx {
				line = tui.HighlightLine(line, i.cfg.Theme)
			}
			out[r-start] = line
		}
	}
	return out
}
