package modes

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/tui"
	"github.com/mattn/go-runewidth"
)

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

	i.mu.Lock()
	i.helpBlock = nil
	i.sessionInfoBlock = block
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
