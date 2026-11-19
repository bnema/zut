package modes

import (
	"context"
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// extractCopyText selects the last quote or fenced code block in source order.
// Blocks are scanned as units so quote markers inside code remain literal.
func extractCopyText(markdown string, all bool) (text string, kind string) {
	text, kind = markdown, "message"
	if all {
		return
	}
	lines := strings.Split(markdown, "\n")
	for n := 0; n < len(lines); {
		line := strings.TrimLeft(lines[n], " ")
		if marker, size := copyFence(line); size >= 3 {
			start := n + 1
			n++
			for n < len(lines) {
				closer := strings.TrimSpace(lines[n])
				m, count := copyFence(closer)
				if m == marker && count >= size && strings.TrimSpace(closer[count:]) == "" {
					break
				}
				n++
			}
			text, kind = strings.Join(lines[start:n], "\n"), "code block"
			if n < len(lines) {
				n++
			}
		} else if strings.HasPrefix(line, ">") {
			var quote []string
			for n < len(lines) {
				line = strings.TrimLeft(lines[n], " ")
				if !strings.HasPrefix(line, ">") {
					break
				}
				quote = append(quote, strings.TrimPrefix(line[1:], " "))
				n++
			}
			text, kind = strings.Join(quote, "\n"), "quote block"
		} else {
			n++
		}
	}
	return
}

func copyFence(line string) (byte, int) {
	if len(line) == 0 || (line[0] != '`' && line[0] != '~') {
		return 0, 0
	}
	n := 0
	for n < len(line) && line[n] == line[0] {
		n++
	}
	return line[0], n
}

func lastAssistantCopyText(messages []provider.Message) (string, bool) {
	for n := len(messages) - 1; n >= 0; n-- {
		if messages[n].Role != provider.RoleAssistant {
			continue
		}
		var text strings.Builder
		for _, content := range messages[n].Content {
			if block, ok := content.(provider.TextBlock); ok {
				text.WriteString(block.Text)
			}
		}
		return text.String(), true
	}
	return "", false
}

func (i *Interactive) runCopy(ctx context.Context, args []string) {
	i.mu.Lock()
	i.statusOK, i.statusErr = "", ""
	if len(args) > 1 || (len(args) == 1 && args[0] != "all") {
		i.statusErr = "usage: /copy [all]"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	messages, _, _ := i.portTranscriptLocked()
	markdown, ok := lastAssistantCopyText(messages)
	term := i.cfg.Terminal
	if !ok {
		i.statusErr = "nothing to copy"
	}
	i.mu.Unlock()
	if !ok {
		i.invalidate()
		return
	}
	text, kind := extractCopyText(markdown, len(args) == 1)
	// Match login URL copying: prefer the remote terminal clipboard, falling
	// back to the system clipboard if OSC 52 is unavailable or fails.
	var err error
	if term == nil || !tui.SupportsOSC52Clipboard() || tui.WriteClipboardTextOSC52(term, text) != nil {
		err = tui.WriteClipboardText(ctx, text)
	}
	i.mu.Lock()
	if err != nil {
		i.statusErr, i.statusOK = "copy: "+err.Error(), ""
	} else {
		i.statusErr = ""
		i.statusOK = "copied " + kind
		if kind != "message" {
			i.statusOK += fmt.Sprintf(" (%d lines)", strings.Count(text, "\n")+1)
		}
	}
	i.mu.Unlock()
	i.invalidate()
}
