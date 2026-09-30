package modes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func timelineTestData() timelineData {
	started := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	finished := started.Add(1250 * time.Millisecond)
	return timelineData{
		System: "You are zot.",
		Tools: []provider.Tool{{
			Name: "bash", Schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`),
		}},
		Messages: []provider.Message{
			{Role: provider.RoleUser, Time: started.Add(-time.Second), Content: []provider.Content{provider.TextBlock{Text: "list files"}}},
			{Role: provider.RoleAssistant, Time: started, Content: []provider.Content{
				provider.TextBlock{Text: "I will inspect them."},
				provider.ToolCallBlock{ID: "call-1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls -la"}`)},
			}},
			{Role: provider.RoleTool, Time: finished, Content: []provider.Content{
				provider.ToolResultBlock{CallID: "call-1", Content: []provider.Content{provider.TextBlock{Text: "README.md"}}},
			}},
		},
		ContextUsed: 1200,
		ContextMax:  200000,
	}
}

func TestSessionTimelineOpensAlternateView(t *testing.T) {
	interactive := &Interactive{timeline: newTimelineView()}
	interactive.runSlash(context.Background(), "/session timeline")
	if !interactive.timeline.Active() {
		t.Fatal("/session timeline did not open the timeline view")
	}
}

func TestSessionPickerIncludesTimeline(t *testing.T) {
	interactive := &Interactive{sessionOpsDialog: newSessionOpsDialog()}
	interactive.openSessionOpsDialog()
	if len(interactive.sessionOpsDialog.items) == 0 || interactive.sessionOpsDialog.items[0].action != "timeline" {
		t.Fatalf("session actions = %+v, want timeline first", interactive.sessionOpsDialog.items)
	}
}

func TestBuildTimelineEntriesPairsToolDetails(t *testing.T) {
	entries := buildTimelineEntries(timelineTestData())
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want system, user, assistant, and tool", len(entries))
	}
	tool := entries[3]
	if tool.Kind != "tool" || tool.ToolCall != "call-1" {
		t.Fatalf("tool entry = %+v", tool)
	}
	if !strings.Contains(tool.Payload, "ls -la") || !strings.Contains(tool.Result, "README.md") {
		t.Fatalf("tool details missing: %+v", tool)
	}
	if !strings.Contains(tool.Schema, "command") {
		t.Fatalf("tool schema missing: %q", tool.Schema)
	}
	if tool.Duration != "1.25s" {
		t.Fatalf("duration = %q, want 1.25s", tool.Duration)
	}
}

func TestTimelineReasoningIsReadableAndScrollable(t *testing.T) {
	data := timelineTestData()
	data.Messages[1].Content = append([]provider.Content{provider.ReasoningBlock{
		Summary: "**First** step\n" + strings.Repeat("A long line of thought. ", 25) + "\nFinal insight",
	}}, data.Messages[1].Content...)
	entries := buildTimelineEntries(data)
	if len(entries) != 5 || entries[2].Kind != "reasoning" || entries[3].Kind != "assistant" {
		t.Fatalf("reasoning entries = %+v", entries)
	}
	if strings.Contains(entries[3].Summary, "First") {
		t.Fatalf("reasoning duplicated in assistant: %+v", entries[3])
	}
	view := newTimelineView()
	view.Open(data)
	view.cursor = 2
	rows := timelineDetailRows(tui.Dark, 40, entries[2], 0)
	if len(rows) < 4 || !strings.Contains(stripANSIBytes(strings.Join(rows, "\n")), "Final insight") {
		t.Fatalf("reasoning detail not complete: %q", rows)
	}
	for _, row := range rows {
		if runewidth.StringWidth(stripANSIBytes(row)) > 40 {
			t.Fatalf("row exceeds width: %q", row)
		}
	}
	first := strings.Join(view.Render(tui.Dark, 40, 17, data), "\n")
	view.HandleKey(tui.Key{Kind: tui.KeyEnd}, data)
	last := strings.Join(view.Render(tui.Dark, 40, 17, data), "\n")
	if view.detailTop == 0 || first == last || !strings.Contains(stripANSIBytes(last), "Final insight") {
		t.Fatalf("detail did not scroll to end: top=%d, output=%q", view.detailTop, last)
	}
	view.HandleKey(tui.Key{Kind: tui.KeyLeft}, data)
	if view.detailTop == 0 {
		t.Fatal("left did not scroll detail up")
	}
	view.HandleKey(tui.Key{Kind: tui.KeyTab}, data)
	if view.detailTop != 0 {
		t.Fatal("changing tab did not reset detail scroll")
	}
}

func TestTimelineNavigationKeysMoveSelection(t *testing.T) {
	data := timelineTestData()
	view := newTimelineView()
	view.Open(data)
	last := len(buildTimelineEntries(data)) - 1
	if view.cursor != last {
		t.Fatalf("initial cursor = %d, want %d", view.cursor, last)
	}
	view.HandleKey(tui.Key{Kind: tui.KeyUp}, data)
	if view.cursor != last-1 {
		t.Fatalf("cursor after up = %d, want %d", view.cursor, last-1)
	}
	view.HandleKey(tui.Key{Kind: tui.KeyDown}, data)
	if view.cursor != last {
		t.Fatalf("cursor after down = %d, want %d", view.cursor, last)
	}
	view.HandleKey(tui.Key{Kind: tui.KeyPageUp}, data)
	if view.cursor != 0 {
		t.Fatalf("cursor after page up = %d, want 0", view.cursor)
	}
	view.HandleKey(tui.Key{Kind: tui.KeyPageDown}, data)
	if view.cursor != last {
		t.Fatalf("cursor after page down = %d, want %d", view.cursor, last)
	}
}

func TestTimelineSearchAndTabs(t *testing.T) {
	data := timelineTestData()
	view := newTimelineView()
	view.Open(data)
	view.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: '/'}, data)
	for _, r := range "readme" {
		view.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: r}, data)
	}
	matches := filterTimelineEntries(buildTimelineEntries(data), view.filter)
	if len(matches) != 1 || matches[0].Kind != "tool" {
		t.Fatalf("matches = %+v, want tool result", matches)
	}
	view.HandleKey(tui.Key{Kind: tui.KeyTab}, data)
	if view.tab != 1 {
		t.Fatalf("tab = %d, want payload tab", view.tab)
	}
	view.HandleKey(tui.Key{Kind: tui.KeyEsc}, data)
	if view.filter != "" || !view.Active() {
		t.Fatal("first esc should clear search without closing timeline")
	}
}

func TestTimelineTabsKeepSelectedTabVisible(t *testing.T) {
	const width = 30
	for selected, name := range timelineTabNames {
		rendered := renderTimelineTabs(tui.Theme{}, width, selected)
		plain := stripANSIBytes(rendered)
		if !strings.Contains(plain, "["+name+"]") {
			t.Errorf("selected tab %q not visible in %q", name, plain)
		}
		if got := runewidth.StringWidth(plain); got > width {
			t.Errorf("tabs width = %d, want <= %d: %q", got, width, plain)
		}
	}
}

func TestTimelineRenderShowsContextAndControls(t *testing.T) {
	data := timelineTestData()
	if contextLines := renderTimelineContext(tui.Theme{}, 100, data); len(contextLines) != 2 {
		t.Fatalf("context rows = %d, want usage and composition only", len(contextLines))
	}
	view := newTimelineView()
	view.Open(data)
	rendered := strings.Join(view.Render(tui.Theme{}, 100, 40, data), "\n")
	for _, want := range []string{"timeline", "context 1.2k / 200.0k", "estimated composition", "ctrl+e export", "TOOL bash"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render missing %q:\n%s", want, rendered)
		}
	}
}

func TestTimelineExportOmitsImageDataAndUsesPrivatePermissions(t *testing.T) {
	data := timelineTestData()
	data.Messages = append(data.Messages, provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.ImageBlock{MimeType: "image/png", Data: []byte("secret-image-bytes")}},
	})
	view := newTimelineView()
	path, err := view.Export(t.TempDir(), data)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "secret-image-bytes") {
		t.Fatal("export embedded image data")
	}
	if !strings.Contains(string(contents), "image/png") {
		t.Fatal("export omitted image metadata")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestTimelinePairsReusedToolCallIDs(t *testing.T) {
	t0 := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	call := func(cmd string) provider.Content {
		return provider.ToolCallBlock{ID: "reused", Name: "bash", Arguments: json.RawMessage(`{"command":"` + cmd + `"}`)}
	}
	result := func(text string, isErr bool) provider.Message {
		return provider.Message{Role: provider.RoleTool, Time: t0.Add(time.Second), Content: []provider.Content{
			provider.ToolResultBlock{CallID: "reused", IsError: isErr, Content: []provider.Content{provider.TextBlock{Text: text}}},
		}}
	}
	data := timelineData{Messages: []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "go"}}},
		{Role: provider.RoleAssistant, Time: t0, Content: []provider.Content{call("first")}},
		result("first-output", false),
		{Role: provider.RoleAssistant, Time: t0.Add(2 * time.Second), Content: []provider.Content{call("second")}},
		result("second-output", true),
		{Role: provider.RoleAssistant, Time: t0.Add(4 * time.Second), Content: []provider.Content{call("third")}},
	}}
	var tools []timelineEntry
	for _, e := range buildTimelineEntries(data) {
		if e.Kind == "tool" {
			tools = append(tools, e)
		}
	}
	if len(tools) != 3 {
		t.Fatalf("tool entries = %d, want 3: %+v", len(tools), tools)
	}
	if !strings.Contains(tools[0].Result, "first-output") || tools[0].IsError {
		t.Fatalf("first call paired wrongly: %+v", tools[0])
	}
	if !strings.Contains(tools[1].Result, "second-output") || !tools[1].IsError {
		t.Fatalf("second call paired wrongly: %+v", tools[1])
	}
	if tools[2].Result != "" || timelineStatus(tools[2]) != "result unavailable" {
		t.Fatalf("third call must stay unpaired: %+v", tools[2])
	}
}

func TestTimelineKeepsOrphanResultVisible(t *testing.T) {
	data := timelineData{Messages: []provider.Message{{Role: provider.RoleTool, Content: []provider.Content{
		provider.ToolResultBlock{CallID: "gone", Content: []provider.Content{provider.TextBlock{Text: "late"}}},
	}}}}
	entries := buildTimelineEntries(data)
	if len(entries) != 1 || !strings.Contains(entries[0].Result, "late") || !strings.Contains(entries[0].Summary, "unpaired") {
		t.Fatalf("orphan result = %+v", entries)
	}
}

func TestTimelineNeverShowsOrExportsEncryptedReasoningOrSignatures(t *testing.T) {
	data := timelineTestData()
	data.Messages[1].Content = append([]provider.Content{
		provider.ReasoningBlock{ID: "r1", Summary: "visible summary", Encrypted: "ENCRYPTED-SECRET-BLOB"},
		provider.TextBlock{Text: "answer", ThoughtSignature: "SIGNATURE-SECRET"},
	}, data.Messages[1].Content...)
	view := newTimelineView()
	view.Open(data)
	var shown strings.Builder
	for tab := 0; tab < timelineTabCount; tab++ {
		view.tab = tab
		for cursor := range buildTimelineEntries(data) {
			view.cursor = cursor
			shown.WriteString(strings.Join(view.Render(tui.Dark, 100, 40, data), "\n"))
		}
	}
	path, err := view.Export(t.TempDir(), data)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"ENCRYPTED-SECRET-BLOB", "SIGNATURE-SECRET"} {
		if strings.Contains(shown.String(), secret) || strings.Contains(string(exported), secret) {
			t.Fatalf("%s leaked into the timeline", secret)
		}
	}
	if !strings.Contains(string(exported), "visible summary") {
		t.Fatal("reasoning summary missing from export")
	}
}

func TestTimelineExportIsExclusiveAndPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	dir := filepath.Join(t.TempDir(), "exports")
	view := newTimelineView()
	path, err := view.Export(dir, timelineTestData())
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("export dir mode = %v (%v), want 700", info.Mode().Perm(), err)
	}
	// A pre-planted symlink at the target name must not be followed.
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
		t.Fatal("exclusive open unexpectedly succeeded on an existing symlink")
	}
	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Fatalf("victim overwritten: %q", got)
	}
}

func TestTimelineSnapshotHidesHostInternalMessages(t *testing.T) {
	ag := core.NewAgent(nil, "m", "system prompt", nil)
	ag.SetMessages([]provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "real prompt"}}},
		{Role: provider.RoleUser, Meta: map[string]string{goalContinueMetaKey: "true"}, Content: []provider.Content{provider.TextBlock{Text: "hidden goal continuation"}}},
		{Role: provider.RoleUser, Meta: map[string]string{autoCompactContinueMetaKey: "true"}, Content: []provider.Content{provider.TextBlock{Text: "hidden compact continuation"}}},
		{Role: provider.RoleUser, Meta: map[string]string{core.RepetitionGuardMetaKey: "true"}, Content: []provider.Content{provider.TextBlock{Text: "hidden repetition guard"}}},
	})
	i := &Interactive{agent: ag, timeline: newTimelineView()}
	i.mu.Lock()
	data := i.timelineDataLocked()
	i.mu.Unlock()
	if data.System != "system prompt" || len(data.Messages) != 1 {
		t.Fatalf("snapshot = system %q, %d messages", data.System, len(data.Messages))
	}
	for _, e := range buildTimelineEntries(data) {
		if strings.Contains(e.Summary, "hidden") {
			t.Fatalf("internal message leaked: %+v", e)
		}
	}
}

func TestTimelineCacheRebuildsOnlyWhenTranscriptChanges(t *testing.T) {
	ag := core.NewAgent(nil, "m", "sys", nil)
	ag.SetMessages([]provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "one"}}}})
	i := &Interactive{agent: ag, timeline: newTimelineView()}
	i.mu.Lock()
	data := i.timelineDataLocked()
	i.timeline.Open(data)
	first := i.timeline.entries(data)
	again := i.timeline.entries(i.timelineDataLocked())
	i.mu.Unlock()
	if &first[0] != &again[0] {
		t.Fatal("unchanged transcript rebuilt the timeline")
	}
	ag.SetMessages([]provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "two"}}}})
	i.mu.Lock()
	updated := i.timeline.entries(i.timelineDataLocked())
	i.mu.Unlock()
	if !strings.Contains(updated[len(updated)-1].Summary, "two") {
		t.Fatalf("stale timeline after transcript change: %+v", updated)
	}
}

func TestTimelineRendersInsideFloatingPane(t *testing.T) {
	data := timelineTestData()
	i := &Interactive{timeline: newTimelineView()}
	i.timeline.Open(data)
	pane := tui.FloatingPaneMaxRect(100, 30)
	lines := padDialogFrame(i.timeline.Render(tui.Dark, pane.ContentWidth(), pane.ContentHeight(), data))
	title, body, _ := floatingDialogBody(lines)
	if title != "timeline" {
		t.Fatalf("pane title = %q, want timeline", title)
	}
	if len(body) > pane.ContentHeight() {
		t.Fatalf("body has %d rows, pane content height is %d", len(body), pane.ContentHeight())
	}
	for _, row := range body {
		if w := runewidth.StringWidth(stripANSIBytes(row)); w > pane.ContentWidth() {
			t.Fatalf("row width %d exceeds pane %d: %q", w, pane.ContentWidth(), row)
		}
	}
	// A narrow drawer must still fit.
	narrow := tui.FloatingPaneMaxRect(40, 12)
	for _, row := range i.timeline.Render(tui.Dark, narrow.ContentWidth(), narrow.ContentHeight(), data) {
		if w := runewidth.StringWidth(stripANSIBytes(row)); w > narrow.ContentWidth() {
			t.Fatalf("narrow row width %d exceeds %d: %q", w, narrow.ContentWidth(), row)
		}
	}
}

func TestTimelineKeysAreOwnedByTimelineAndCtrlCCloses(t *testing.T) {
	i := NewInteractive(InteractiveConfig{Agent: core.NewAgent(nil, "m", "", nil)})
	i.openTimeline()
	if !i.timeline.Active() || !i.confirmChildActive() {
		t.Fatal("timeline is not registered as an active overlay")
	}
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: 'x'})
	if i.ed.Value() != "" {
		t.Fatalf("key leaked to the main editor: %q", i.ed.Value())
	}
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlC})
	if i.timeline.Active() {
		t.Fatal("ctrl+c did not close the timeline")
	}
}

func hostileTimelineData() timelineData {
	const hostile = "a\x1b]0;evil-title\x07b\x1b[31mred\x1b[0m\tc\rd\x1b"
	data := timelineData{
		System: "sys " + hostile + "\nsecond\r\nthird\rfourth",
		Tools:  []provider.Tool{{Name: "bash", Schema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: hostile + "\nmore"}}},
			{Role: provider.RoleAssistant, Content: []provider.Content{
				provider.ReasoningBlock{Summary: "think " + hostile},
				provider.ToolCallBlock{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"command":"echo \u001b]0;evil-title\u0007 \t x\r y"}`)},
			}},
			{Role: provider.RoleTool, Content: []provider.Content{
				provider.ToolResultBlock{CallID: "c1", Content: []provider.Content{provider.TextBlock{Text: hostile}}},
			}},
		},
	}
	return data
}

func TestTimelineSanitizesUntrustedText(t *testing.T) {
	data := hostileTimelineData()
	view := newTimelineView()
	view.Open(data)
	view.searching = true
	view.filter = "q\x1b]0;evil-title\x07\tz"
	entries := buildTimelineEntries(data)
	view.filter = ""
	for _, width := range []int{20, 60, 120} {
		for tab := 0; tab < timelineTabCount; tab++ {
			view.tab = tab
			for cursor := range entries {
				view.cursor = cursor
				out := strings.Join(view.Render(tui.Dark, width, 30, data), "\n")
				plain := stripANSIBytes(out)
				// JSON payloads show control bytes as inert \uXXXX text, so
				// only raw controls are checked there; the summary tab and the
				// list rows must have the OSC payload removed entirely.
				bad := []string{"\r", "\t", "\a", "\x1b"}
				if tab == 0 {
					bad = append(bad, "evil-title")
				}
				for _, b := range bad {
					if strings.Contains(out, b) && b != "\x1b" || b == "\x1b" && strings.Contains(plain, b) {
						t.Fatalf("width %d tab %d entry %d leaked %q:\n%q", width, tab, cursor, b, plain)
					}
				}
				for _, row := range strings.Split(plain, "\n") {
					if w := runewidth.StringWidth(row); w > width {
						t.Fatalf("width %d tab %d entry %d: row width %d: %q", width, tab, cursor, w, row)
					}
				}
			}
		}
	}
	// A typed or pasted filter cannot inject control sequences either.
	view.searching = true
	view.HandleKey(tui.Key{Kind: tui.KeyPaste, Paste: "x\x1b]0;evil-title\x07\ty"}, data)
	view.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: '\t'}, data)
	view.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: '\r'}, data)
	shown := stripANSIBytes(strings.Join(view.Render(tui.Dark, 80, 30, data), "\n"))
	if strings.ContainsAny(view.filter, "\x1b\a\t\r") || strings.Contains(shown, "evil-title\x07") || strings.ContainsAny(shown, "\a\t\r") {
		t.Fatalf("filter leaked control text: filter=%q", view.filter)
	}
}

func TestTimelineBodyFitsPaneAtEveryHeight(t *testing.T) {
	data := timelineTestData()
	data.Messages = append(data.Messages, provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{
		provider.ReasoningBlock{Summary: strings.Repeat("long reasoning line\n", 40)},
	}})
	view := newTimelineView()
	view.Open(data)
	view.notice = "exported to ~/x.json"
	for _, searching := range []bool{false, true} {
		view.searching = searching
		for height := 1; height <= 40; height++ {
			for _, width := range []int{12, 30, 78, 120} {
				for tab := 0; tab < timelineTabCount; tab++ {
					view.tab = tab
					lines := padDialogFrame(view.Render(tui.Dark, width, height, data))
					_, body, _ := floatingDialogBody(lines)
					if len(body) > height {
						t.Fatalf("height %d width %d tab %d: body has %d rows", height, width, tab, len(body))
					}
					for _, row := range body {
						if w := runewidth.StringWidth(stripANSIBytes(row)); w > width {
							t.Fatalf("height %d width %d: row width %d: %q", height, width, w, row)
						}
					}
				}
			}
		}
	}
	// Tiny panes still show the selected event before optional chrome.
	view.searching = false
	view.tab = 0
	lines := padDialogFrame(view.Render(tui.Dark, 80, 3, data))
	_, body, _ := floatingDialogBody(lines)
	if !strings.Contains(stripANSIBytes(strings.Join(body, "\n")), "REASONING") {
		t.Fatalf("selected event missing from a 3-row pane: %q", body)
	}
}

func TestTimelineSnapshotCacheTracksPromptContentAndSkipsCopies(t *testing.T) {
	ag := core.NewAgent(nil, "m", "prompt-AAAA", core.Registry{})
	ag.SetMessages([]provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "one"}}}})
	i := &Interactive{agent: ag, timeline: newTimelineView()}
	snapshot := func() timelineData {
		i.mu.Lock()
		defer i.mu.Unlock()
		return i.timelineDataLocked()
	}

	first := snapshot()
	again := snapshot()
	if &first.Messages[0] != &again.Messages[0] || !again.Est.ok {
		t.Fatal("unchanged transcript and prompt were re-copied")
	}
	// A same-length system prompt edit changes neither the transcript
	// revision nor the prompt length, yet must refresh the snapshot.
	ag.SetSystemPrompt("prompt-BBBB")
	if edited := snapshot(); edited.System != "prompt-BBBB" {
		t.Fatalf("stale system prompt after same-length edit: %q", edited.System)
	}
	// Same for a tool whose schema changes at equal length.
	ag.SetTools(core.Registry{"t": schemaTool{name: "t", schema: `{"a":1}`}})
	if s := snapshot(); len(s.Tools) != 1 || string(s.Tools[0].Schema) != `{"a":1}` {
		t.Fatalf("tools = %+v", s.Tools)
	}
	ag.SetTools(core.Registry{"t": schemaTool{name: "t", schema: `{"a":2}`}})
	if s := snapshot(); string(s.Tools[0].Schema) != `{"a":2}` {
		t.Fatalf("stale tool schema after same-length edit: %s", s.Tools[0].Schema)
	}
	// The context gauge is live and never cached.
	i.mu.Lock()
	i.lastCtxInput = 42
	i.mu.Unlock()
	if snapshot().ContextUsed != 42 {
		t.Fatal("context usage was served from the cache")
	}
}

type schemaTool struct{ name, schema string }

func (t schemaTool) Name() string            { return t.name }
func (t schemaTool) Description() string     { return "d" }
func (t schemaTool) Schema() json.RawMessage { return json.RawMessage(t.schema) }
func (t schemaTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	return core.ToolResult{}, nil
}
