package modes

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/tui"
	"github.com/mattn/go-runewidth"
)

type sessionInfoBlock struct {
	afterMessage int
	lines        []string
}

// renderSessionInfoBlock formats information owned by the live interactive
// session. Paths remain absolute so users can copy the transcript location.
func renderSessionInfoBlock(th tui.Theme, width int, sessionID, sessionPath, cwd, provider, model string) []string {
	if width < 20 {
		width = 20
	}

	rows := [][2]string{
		{"session id", valueOrUnavailable(sessionID)},
		{"session file", valueOrUnavailable(sessionPath)},
		{"working directory", valueOrUnavailable(cwd)},
		{"provider", valueOrUnavailable(provider)},
		{"model", valueOrUnavailable(model)},
	}
	labelWidth := 14
	for _, row := range rows {
		if n := runewidth.StringWidth(row[0]); n > labelWidth {
			labelWidth = n
		}
	}

	pad := func(label string) string {
		return label + strings.Repeat(" ", labelWidth-runewidth.StringWidth(label))
	}

	out := []string{frameHeader(th, "session info", width), ""}
	for _, row := range rows {
		out = append(out, fmt.Sprintf("  %s  %s",
			th.FGColor(th.Accent, pad(row[0])),
			th.FGColor(th.Muted, row[1])))
	}
	return append(out, "", frameRule(th, width), "")
}

func insertSessionInfoBlocks(rows []string, anchors []tui.MessageAnchor, blocks []sessionInfoBlock) []string {
	if len(blocks) == 0 {
		return rows
	}

	out := make([]string, 0, len(rows))
	rowStart := 0
	for _, block := range blocks {
		insertAt := len(rows)
		if block.afterMessage >= 0 && block.afterMessage < len(anchors) {
			insertAt = anchors[block.afterMessage].Row
		}
		if insertAt < rowStart {
			insertAt = rowStart
		}
		out = append(out, rows[rowStart:insertAt]...)
		out = append(out, block.lines...)
		rowStart = insertAt
	}
	return append(out, rows[rowStart:]...)
}

func clampSessionInfoBlocks(blocks []sessionInfoBlock, messageCount int) {
	for idx := range blocks {
		if blocks[idx].afterMessage > messageCount {
			blocks[idx].afterMessage = messageCount
		}
	}
}

func sessionInfoBlocksKey(blocks []sessionInfoBlock) string {
	var key strings.Builder
	for _, block := range blocks {
		fmt.Fprintf(&key, "%d\n", block.afterMessage)
		for _, line := range block.lines {
			key.WriteString(line)
			key.WriteByte('\n')
		}
	}
	return key.String()
}

func valueOrUnavailable(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unavailable"
	}
	return value
}

func absoluteSessionInfoPath(path string) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}

func (i *Interactive) showSessionInfo() {
	i.mu.Lock()
	agent := i.agent
	pathFn := i.cfg.CurrentSessionPath
	cwd := i.cfg.CWD
	providerName := i.cfg.Provider
	model := i.cfg.Model
	theme := i.cfg.Theme
	terminal := i.cfg.Terminal
	i.mu.Unlock()

	sessionID := sessionID(agent)
	sessionPath := ""
	if pathFn != nil {
		sessionPath = pathFn()
	}

	width := 80
	if terminal != nil {
		width, _ = terminal.Size()
	}
	block := renderSessionInfoBlock(theme, width, sessionID, absoluteSessionInfoPath(sessionPath), absoluteSessionInfoPath(cwd), providerName, model)
	messageCount := 0
	if agent != nil {
		messageCount = len(filterHiddenTranscriptMessages(agent.Messages()))
	}

	i.mu.Lock()
	i.helpBlock = nil
	i.sessionInfoBlocks = append(i.sessionInfoBlocks, sessionInfoBlock{
		afterMessage: messageCount,
		lines:        block,
	})
	i.statusErr = ""
	i.statusOK = ""
	i.scrollOffset = 0
	i.mu.Unlock()
}

func sessionID(agent *core.Agent) string {
	if agent == nil {
		return ""
	}
	return agent.SessionID()
}
