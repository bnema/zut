package modes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/extensions"
	"github.com/bnema/zut/packages/tui"
)

const reloadStatusDuration = 5 * time.Second

func formatReloadStatus(stats extensions.ReloadStats) (string, bool) {
	msg := fmt.Sprintf("reloaded: %d stopped, %d loaded (%d ready)", stats.Stopped, stats.Loaded, stats.Ready)
	if len(stats.Errors) == 0 {
		return msg, false
	}
	details := make([]string, 0, len(stats.Errors))
	for _, err := range stats.Errors {
		if err != nil {
			details = append(details, err.Error())
		}
	}
	msg += fmt.Sprintf(", %d error(s)", len(stats.Errors))
	if len(details) > 0 {
		msg += ": " + strings.Join(details, "; ")
	}
	return msg, true
}

func (i *Interactive) setReloadStatus(msg string, failed bool) uint64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.reloadStatusSeq++
	if failed {
		i.statusOK = ""
		i.statusErr = msg
		i.reloadErrors = append(i.reloadErrors, msg)
		return i.reloadStatusSeq
	}
	i.statusOK = msg
	i.statusErr = ""
	i.reloadErrors = nil
	return i.reloadStatusSeq
}

func renderReloadErrors(theme tui.Theme, errors []string, width int) []string {
	const marker = "✖ "
	const indent = "  "
	wrapWidth := width - len([]rune(marker))
	if wrapWidth < 8 {
		wrapWidth = 8
	}
	var out []string
	for _, msg := range errors {
		// Split explicit newlines before wrapping. Passing a multiline string
		// to WrapANSILine would leave the terminal to interpret the embedded
		// newline, which can misalign the continuation text. Normalize CRLF
		// first so no raw CR survives inside a row.
		for lineIndex, paragraph := range strings.Split(strings.ReplaceAll(msg, "\r\n", "\n"), "\n") {
			wrapped := tui.WrapANSILine(paragraph, wrapWidth)
			if len(wrapped) == 0 {
				wrapped = []string{""}
			}
			for wrapIndex, line := range wrapped {
				prefix := indent
				if lineIndex == 0 && wrapIndex == 0 {
					prefix = marker
				}
				out = append(out, theme.FGColor(theme.Error, prefix+line))
			}
		}
	}
	return out
}

func (i *Interactive) clearReloadStatus(seq uint64, msg string, failed bool) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.reloadStatusSeq != seq {
		return false
	}
	if failed {
		if i.statusErr != msg {
			return false
		}
		i.statusErr = ""
		return true
	}
	if i.statusOK != msg {
		return false
	}
	i.statusOK = ""
	return true
}

func (i *Interactive) dismissReloadStatus(ctx context.Context, seq uint64, msg string, failed bool) {
	timer := time.NewTimer(reloadStatusDuration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		if i.clearReloadStatus(seq, msg, failed) {
			i.invalidate()
		}
	}
}
