package modes

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// portScreen interprets the main-screen VT subset emitted by the renderer.
// Unlike stripping ANSI, this observes erased and overwritten output.
func portScreen(data string) string {
	const rows, cols = 24, 80
	cells := make([][]rune, rows)
	for i := range cells {
		cells[i] = make([]rune, cols)
	}
	x, y := 0, 0
	lf := func() {
		y++
		if y == rows {
			copy(cells, cells[1:])
			cells[rows-1] = make([]rune, cols)
			y--
		}
	}
	for len(data) > 0 {
		if strings.HasPrefix(data, "\x1b[") {
			end := 2
			for end < len(data) && (data[end] < 0x40 || data[end] > 0x7e) {
				end++
			}
			if end == len(data) {
				break
			}
			args, cmd := data[2:end], data[end]
			data = data[end+1:]
			parts := strings.Split(args, ";")
			n, _ := strconv.Atoi(parts[0])
			if n == 0 {
				n = 1
			}
			switch cmd {
			case 'A':
				y = max(0, y-n)
			case 'B':
				y = min(rows-1, y+n)
			case 'C':
				x = min(cols-1, x+n)
			case 'H':
				y = min(rows-1, max(0, n-1))
				x = 0
				if len(parts) > 1 {
					v, _ := strconv.Atoi(parts[1])
					x = min(cols-1, max(0, v-1))
				}
			case 'J':
				mode, _ := strconv.Atoi(args)
				switch mode {
				case 2:
					for i := range cells {
						clear(cells[i])
					}
				case 0:
					clear(cells[y][min(x, cols):])
					for i := y + 1; i < rows; i++ {
						clear(cells[i])
					}
				}
			case 'K':
				mode, _ := strconv.Atoi(args)
				if mode == 2 {
					clear(cells[y])
				} else {
					clear(cells[y][min(x, cols):])
				}
			}
			continue
		}
		if strings.HasPrefix(data, "\x1b]") || strings.HasPrefix(data, "\x1b_") {
			end := strings.Index(data, "\x1b\\")
			bell := strings.IndexByte(data, 7)
			if bell >= 0 && (end < 0 || bell < end) {
				data = data[bell+1:]
			} else if end >= 0 {
				data = data[end+2:]
			} else {
				break
			}
			continue
		}
		r, n := utf8.DecodeRuneInString(data)
		data = data[n:]
		switch r {
		case '\r':
			x = 0
		case '\n':
			lf()
		default:
			if r < 32 {
				continue
			}
			w := runewidth.RuneWidth(r)
			if w == 0 {
				continue
			}
			if x+w > cols {
				x = 0
				lf()
			}
			cells[y][x] = r
			x += w
		}
	}
	var out strings.Builder
	for _, row := range cells {
		for _, r := range row {
			if r == 0 {
				r = ' '
			}
			out.WriteRune(r)
		}
		out.WriteByte('\n')
	}
	return out.String()
}

func TestInteractivePortReplyRemainsOnScreen(t *testing.T) {
	for _, history := range []int{0, 40} {
		t.Run(fmt.Sprintf("history_%d", history), func(t *testing.T) {
			port := sessionmocks.NewMockPort(t)
			events := make(chan session.Event, 32)
			var messages []provider.Message
			for n := 0; n < history; n++ {
				messages = append(messages, portUserMessage(fmt.Sprintf("history line %d", n)))
			}
			port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", Run: session.RunStateIdle}, Messages: messages, Seq: 1}, nil).Once()
			expectPortSubscribe(port, events, nil)
			r := startPortInteractive(t, port, nil)
			seq := uint64(1)
			send := func(ev session.Event) { seq++; ev.Seq = seq; events <- ev }
			send(session.Event{Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{ID: "s1", Run: session.RunStateRunning}}})
			send(session.Event{Kind: session.EvAgent, Agent: core.EvUserMessage{Message: portUserMessage("askprompt")}})
			send(session.Event{Kind: session.EvAgent, Agent: core.EvAssistantStart{}})
			args := json.RawMessage(`{"command":"ls"}`)
			call := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "c1", Name: "bash", Arguments: args}}}
			send(session.Event{Kind: session.EvAgent, Agent: core.EvToolUseStart{ID: "c1", Name: "bash"}})
			send(session.Event{Kind: session.EvAgent, Agent: core.EvAssistantMessage{Message: call}})
			send(session.Event{Kind: session.EvAgent, Agent: core.EvTurnEnd{Stop: provider.StopToolUse}})
			send(session.Event{Kind: session.EvAgent, Agent: core.EvToolCall{ID: "c1", Name: "bash", Args: args}})
			send(session.Event{Kind: session.EvAgent, Agent: core.EvToolResult{ID: "c1", Result: core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "out"}}}}})
			send(session.Event{Kind: session.EvAgent, Agent: core.EvAssistantStart{}})
			reply := "closingwombat final reply"
			for _, delta := range []string{"closingwombat ", "final reply"} {
				send(session.Event{Kind: session.EvAgent, Agent: core.EvTextDelta{Delta: delta}})
			}
			r.waitOutput(t, "closingwombat")
			send(session.Event{Kind: session.EvAgent, Agent: core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: reply}}}}})
			send(session.Event{Kind: session.EvAgent, Agent: core.EvDone{}})
			idle := session.Event{Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{ID: "s1", Run: session.RunStateIdle}}}
			send(idle)
			require.Eventually(t, func() bool {
				r.i.mu.Lock()
				defer r.i.mu.Unlock()
				return r.i.lastSeq == seq && !r.i.busy && !r.i.stream.Active()
			}, 5*time.Second, 10*time.Millisecond)
			// A redundant post-idle status update must keep the reply painted.
			send(idle)
			require.Eventually(t, func() bool { r.i.mu.Lock(); defer r.i.mu.Unlock(); return r.i.lastSeq == seq }, time.Second, 10*time.Millisecond)
			require.Eventually(t, func() bool { return strings.Contains(portScreen(r.term.String()), reply) }, time.Second, 10*time.Millisecond, "settled screen:\n%s", portScreen(r.term.String()))
			r.i.mu.Lock()
			snapshot := session.Snapshot{Info: r.i.portInfo, Messages: append([]provider.Message(nil), r.i.portMsgs...), Seq: seq + 1}
			r.i.mu.Unlock()
			port.EXPECT().Snapshot(mock.Anything).Return(snapshot, nil).Once()
			send(session.Event{Kind: session.EvMessagesReplaced, Data: session.MessagesReplacedEvent{}})
			require.Eventually(t, func() bool { r.i.mu.Lock(); defer r.i.mu.Unlock(); return r.i.lastSeq == seq }, time.Second, 10*time.Millisecond)
			require.Eventually(t, func() bool { return strings.Contains(portScreen(r.term.String()), reply) }, time.Second, 10*time.Millisecond)
			screen := portScreen(r.term.String())
			t.Logf("settled screen after resnapshot:\n%s", screen)
			for _, count := range []int{1, 2} {
				r.i.mu.Lock()
				r.i.statusOK = "15 words · ChatGPT · 0.6 s"
				if count == 1 {
					r.i.statusOK = ""
					r.i.extNotes = []string{"voice status"}
				}
				r.i.mu.Unlock()
				r.i.invalidate()
				marker := "15 words"
				if count == 1 {
					marker = "voice status"
				}
				require.Eventually(t, func() bool { return strings.Contains(portScreen(r.term.String()), marker) }, time.Second, 10*time.Millisecond)
				r.i.mu.Lock()
				r.i.statusOK = ""
				r.i.extNotes = nil
				r.i.mu.Unlock()
				r.i.invalidate()
				require.Eventually(t, func() bool { return !strings.Contains(portScreen(r.term.String()), marker) }, time.Second, 10*time.Millisecond)
				require.Contains(t, portScreen(r.term.String()), reply, "after removing %d status rows", count)
			}
		})
	}
}

func TestDrawLogStatusShrinkScreen(t *testing.T) {
	for _, height := range []int{4, 20, 40} {
		for _, shrink := range []int{1, 2} {
			t.Run(fmt.Sprintf("height_%d_shrink_%d", height, shrink), func(t *testing.T) {
				var out strings.Builder
				r := tui.NewRenderer(&out)
				r.Resize(80, 24)
				chat := make([]string, height)
				for n := range chat {
					chat[n] = fmt.Sprintf("transcript %d", n)
				}
				chat[len(chat)-1] = "closingwombat final reply"
				bottom := []string{"status", "editor"}
				r.DrawLog(chat, bottom, 1, 0)
				expanded := append([]string(nil), bottom...)
				for range shrink {
					expanded = append([]string{"voice status"}, expanded...)
				}
				r.DrawLog(chat, expanded, 1+shrink, 0)
				r.DrawLog(chat, bottom, 1, 0)
				require.Contains(t, portScreen(out.String()), "closingwombat final reply")
				// A later unchanged paint must remain safe as well.
				r.DrawLog(chat, bottom, 1, 0)
				require.Contains(t, portScreen(out.String()), "closingwombat final reply")
			})
		}
	}
}

func TestDrawLogStatusShrinkAllHeights(t *testing.T) {
	for variant := 0; variant < 4; variant++ {
		t.Run(fmt.Sprint(variant), func(t *testing.T) {
			if variant&2 != 0 {
				t.Setenv("TERM_PROGRAM", "vscode")
			} else {
				t.Setenv("TERM_PROGRAM", "")
			}
			for height := 1; height < 60; height++ {
				for shrink := 1; shrink <= 2; shrink++ {
					for _, inChat := range []bool{false, true} {
						var out strings.Builder
						r := tui.NewRenderer(&out)
						if variant&1 != 0 {
							bg := tui.Color256(237)
							th := tui.Dark
							th.Background = &bg
							r.SetTheme(th)
						}
						r.Resize(80, 24)
						chat := make([]string, height)
						for n := range chat {
							chat[n] = fmt.Sprintf("transcript %d", n)
						}
						chat[height-1] = "closingwombat final reply"
						bottom := []string{"", ""}
						expanded := append([]string(nil), bottom...)
						expandedChat := append([]string(nil), chat...)
						for range shrink {
							if inChat {
								expandedChat = append(expandedChat, "voice status")
							} else {
								expanded = append([]string{"voice status"}, expanded...)
							}
						}
						r.DrawLog(chat, bottom, 0, 0)
						r.DrawLog(expandedChat, expanded, 0, 0)
						r.DrawLog(chat, bottom, 0, 0)
						if !strings.Contains(portScreen(out.String()), "closingwombat final reply") {
							t.Fatalf("height=%d shrink=%d inChat=%v\n%s\nbytes:%q", height, shrink, inChat, portScreen(out.String()), out.String())
						}
					}
				}
			}
		})
	}
}
