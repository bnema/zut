package modes

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestSessionTreeAsyncLoadOpensImmediatelyAndStreamsNewestFirst(t *testing.T) {
	d := newSessionTreeDialog()
	started := make(chan struct{})
	release := make(chan struct{})
	d.loadFamily = func(context.Context, string, string, string) ([]sessionTreeItem, error) {
		close(started)
		<-release
		return []sessionTreeItem{{label: "oldest"}, {label: "newest"}}, nil
	}

	events := d.OpenSessionFamilyAsync(context.Background(), "root", "cwd", "current", nil)
	if !d.Active() || !d.loading {
		t.Fatal("tree was not visible and loading immediately")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background tree loader did not start")
	}
	if len(d.items) != 0 {
		t.Fatalf("tree showed items before the loader released them: %#v", d.items)
	}

	close(release)
	if err := applySessionTreeLoads(d, events); err != nil {
		t.Fatalf("apply background tree load: %v", err)
	}
	if d.loading {
		t.Fatal("tree remained loading after its terminal event")
	}
	if got := []string{d.items[0].label, d.items[1].label}; !reflect.DeepEqual(got, []string{"newest", "oldest"}) {
		t.Fatalf("stream order = %v, want newest to oldest", got)
	}
}

func TestSessionTreeAsyncLoadCancelsWhenClosed(t *testing.T) {
	d := newSessionTreeDialog()
	started := make(chan struct{})
	d.loadFamily = func(ctx context.Context, _, _, _ string) ([]sessionTreeItem, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	events := d.OpenSessionFamilyAsync(context.Background(), "root", "cwd", "current", nil)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background tree loader did not start")
	}
	select {
	case event := <-events:
		if event.kind != sessionTreeLoadStarted {
			t.Fatalf("first load event = %v, want started", event.kind)
		}
		if err := d.ApplyLoad(event); err != nil {
			t.Fatalf("apply startup event: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tree loader did not announce startup")
	}
	d.Close()
	if d.Active() {
		t.Fatal("Close left the tree active")
	}
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("canceled tree loader emitted an event")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled tree loader did not stop")
	}
}

func TestSessionTreeTargetCarriesEffectiveBoundaryAndDraft(t *testing.T) {
	msgs := []provider.Message{
		treeTestMessage(provider.RoleUser, provider.TextBlock{Text: "first"}),
		treeTestMessage(provider.RoleAssistant, provider.TextBlock{Text: "answer"}),
		treeTestMessage(provider.RoleUser,
			provider.TextBlock{Text: "line one"},
			provider.ImageBlock{MimeType: "image/png", Data: []byte{1, 2}},
			provider.TextBlock{Text: "line two"}),
	}

	d := newSessionTreeDialog()
	if !d.OpenMessages(msgs) {
		t.Fatal("OpenMessages returned false")
	}
	d.cursor = 2
	act := d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if !act.Select {
		t.Fatal("enter did not select a target")
	}
	got := act.Target
	if got.SourcePath != "" || got.EffectiveIndex != 2 || got.SelectionBoundary != 2 {
		t.Fatalf("target indices/path = %+v", got)
	}
	if got.Role != provider.RoleUser || got.Boundary != sessionTreeMessageBoundary {
		t.Fatalf("target role/boundary = %+v", got)
	}
	if got.UserDraft != "line one\nline two" {
		t.Fatalf("user draft = %q", got.UserDraft)
	}
	// The scalar fields remain usable by the current integration layer.
	if act.MessageIdx != 2 || act.Role != provider.RoleUser || act.Prompt != got.UserDraft {
		t.Fatalf("legacy action fields = %+v", act)
	}
}

func TestSessionTreeRetainsPreCompactionMessagesAndHidesToolRows(t *testing.T) {
	root := t.TempDir()
	cwd := "/workspace/tree-history"
	session, err := core.NewSession(root, cwd, "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []provider.Message{
		treeTestMessage(provider.RoleUser, provider.TextBlock{Text: "old prompt"}),
		treeTestMessage(provider.RoleAssistant, provider.TextBlock{Text: "old answer"}),
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "call-1", Name: "bash"}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "call-1", Content: []provider.Content{provider.TextBlock{Text: "tool output"}}}}},
		treeTestMessage(provider.RoleAssistant, provider.TextBlock{Text: "old final"}),
	} {
		if err := session.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.AppendCompaction([]provider.Message{
		{
			Role:    provider.RoleUser,
			Meta:    map[string]string{"compaction": "true"},
			Content: []provider.Content{provider.TextBlock{Text: "summary"}},
		},
		treeTestMessage(provider.RoleAssistant, provider.TextBlock{Text: "old final"}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(treeTestMessage(provider.RoleAssistant, provider.TextBlock{Text: "new answer"})); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Meta:    map[string]string{shellEscapeMetaKey: "true"},
		Content: []provider.Content{provider.TextBlock{Text: "shell output"}},
	}); err != nil {
		t.Fatal(err)
	}
	path := session.Path
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	d := newSessionTreeDialog()
	if !d.OpenSessionFamily(root, cwd, path) {
		t.Fatal("OpenSessionFamily returned false")
	}
	var oldPrompt, newAnswer *sessionTreeItem
	oldFinalRows := 0
	for idx := range d.items {
		item := &d.items[idx]
		if strings.Contains(item.label, "old prompt") {
			oldPrompt = item
		}
		if strings.Contains(item.label, "new answer") {
			newAnswer = item
		}
		if strings.Contains(item.label, "old final") {
			oldFinalRows++
		}
		if strings.Contains(item.label, "tool") || strings.Contains(item.label, "result") || strings.Contains(item.label, "summary") || strings.Contains(item.label, "shell output") {
			t.Fatalf("internal/tool row leaked into fork picker: %q", item.label)
		}
	}
	if oldPrompt == nil || !oldPrompt.target.Historical {
		t.Fatalf("old pre-compaction prompt target = %+v, want historical target", oldPrompt)
	}
	if newAnswer == nil || newAnswer.target.Historical {
		t.Fatalf("current answer target = %+v, want effective target", newAnswer)
	}
	if oldFinalRows != 1 {
		t.Fatalf("preserved compaction-tail row rendered %d times, want once", oldFinalRows)
	}
}

func TestSessionTreeOpensWithExtensionStateRows(t *testing.T) {
	root := t.TempDir()
	cwd := "/workspace/tree-extension-state"
	session, err := core.NewSession(root, cwd, "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(treeTestMessage(provider.RoleUser, provider.TextBlock{Text: "prompt"})); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendExtensionState("tasked-phases", json.RawMessage(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(treeTestMessage(provider.RoleAssistant, provider.TextBlock{Text: "answer"})); err != nil {
		t.Fatal(err)
	}
	path := session.Path
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	d := newSessionTreeDialog()
	if !d.OpenSessionFamily(root, cwd, path) {
		t.Fatal("OpenSessionFamily returned false for a session with extension state")
	}
}

func TestSessionTreeHistoricalSelectionCreatesBranchFromOldSegment(t *testing.T) {
	root := t.TempDir()
	cwd := "/workspace/tree-history-branch"
	sess, err := core.NewSession(root, cwd, "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(treeTestMessage(provider.RoleUser, provider.TextBlock{Text: "old prompt"})); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(treeTestMessage(provider.RoleAssistant, provider.TextBlock{Text: "old answer"})); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendCompaction([]provider.Message{{
		Role:    provider.RoleUser,
		Meta:    map[string]string{"compaction": "true"},
		Content: []provider.Content{provider.TextBlock{Text: "summary"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(treeTestMessage(provider.RoleAssistant, provider.TextBlock{Text: "new answer"})); err != nil {
		t.Fatal(err)
	}
	path := sess.Path
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	d := newSessionTreeDialog()
	if !d.OpenSessionFamily(root, cwd, path) {
		t.Fatal("OpenSessionFamily returned false")
	}
	var target sessionTreeTarget
	found := false
	for _, item := range d.items {
		if strings.Contains(item.label, "old answer") {
			target = item.target
			found = true
			break
		}
	}
	if !found || !target.Historical {
		t.Fatalf("old target = %+v, found=%v", target, found)
	}

	// Historical rows ask the daemon to fork the selected historical era;
	// the daemon retains ownership of branch policy and materialization.
	i, _, client, _ := newTreePortInteractiveAt(t, root, cwd, path)
	nextPort := sessionmocks.NewMockPort(t)
	var createdSpec session.CreateSpec
	client.EXPECT().Create(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, spec session.CreateSpec) (session.Info, error) {
		createdSpec = spec
		return session.Info{ID: "branch"}, nil
	}).Once()
	client.EXPECT().Attach(mock.Anything, session.ID("branch")).Return(nextPort, nil).Once()
	nextPort.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "branch", CWD: cwd, Run: session.RunStateIdle}}, nil).Once()
	nextPort.EXPECT().Subscribe(mock.Anything, mock.Anything).Return(make(chan session.Event), nil).Once()
	i.applySessionTreeTarget(target, 1)
	awaitSessionSwitch(t, i)
	if createdSpec.ResumePath != "" || createdSpec.Fork == nil {
		t.Fatalf("historical selection spec = %+v, want daemon fork", createdSpec)
	}
	fork := createdSpec.Fork
	if fork.ParentPath != path || !fork.Hidden || fork.HistorySegment == nil || *fork.HistorySegment != target.HistorySegment || fork.UpToMessage != 2 {
		t.Fatalf("historical fork = %+v, want old segment through answer", fork)
	}

}

func TestSessionTreeUsesCurrentFamilyAndNoRootFallback(t *testing.T) {
	root := t.TempDir()
	cwd := "/workspace/current-family"
	first := newTreeTestSession(t, root, cwd, []provider.Message{
		treeTestMessage(provider.RoleUser, provider.TextBlock{Text: "unrelated root"}),
	})
	unrelatedFile, err := os.OpenFile(first, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unrelatedFile.WriteString("{unrelated corruption}\n"); err != nil {
		_ = unrelatedFile.Close()
		t.Fatal(err)
	}
	if err := unrelatedFile.Close(); err != nil {
		t.Fatal(err)
	}
	current := newTreeTestSession(t, root, cwd, []provider.Message{
		treeTestMessage(provider.RoleUser, provider.TextBlock{Text: "current root"}),
	})

	d := newSessionTreeDialog()
	if !d.OpenSessionFamily(root, cwd, current) {
		t.Fatal("OpenSessionFamily returned false")
	}
	for _, item := range d.items {
		if item.path != current {
			t.Fatalf("item from unrelated family: %+v (first root %q)", item, first)
		}
	}
	if !d.Active() {
		t.Fatal("dialog is not active")
	}
	oldItems := append([]sessionTreeItem(nil), d.items...)
	if d.OpenSessionFamily(root, cwd, t.TempDir()+"/not-a-session.jsonl") {
		t.Fatal("missing current path selected a forest root")
	}
	if !d.Active() || len(d.items) != len(oldItems) || d.items[0].path != oldItems[0].path {
		t.Fatal("failed open changed the active dialog")
	}
}

func TestSessionTreeShowsEmptyAndDetachedBoundaries(t *testing.T) {
	root := t.TempDir()
	cwd := "/workspace/stale-compaction"
	parent := newTreeTestSession(t, root, cwd, []provider.Message{
		treeTestMessage(provider.RoleUser, provider.TextBlock{Text: "one"}),
		treeTestMessage(provider.RoleAssistant, provider.TextBlock{Text: "two"}),
		treeTestMessage(provider.RoleUser, provider.TextBlock{Text: "three"}),
	})

	// Both children are created against the old effective parent. The first
	// has no post-fork rows; the second's fork point becomes stale after the
	// parent is compacted to one message.
	emptyChild, err := core.BranchSessionHidden(parent, root, cwd, "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	staleChild, err := core.BranchSessionHidden(parent, root, cwd, "test", 3)
	if err != nil {
		t.Fatal(err)
	}
	parentSession, _, err := core.OpenSession(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := parentSession.AppendCompaction([]provider.Message{
		treeTestMessage(provider.RoleUser, provider.TextBlock{Text: "compacted"}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := parentSession.Close(); err != nil {
		t.Fatal(err)
	}

	d := newSessionTreeDialog()
	if !d.OpenSessionFamily(root, cwd, parent) {
		t.Fatal("OpenSessionFamily returned false after compaction")
	}
	var empty, detached *sessionTreeItem
	staleMessageRows := 0
	for i := range d.items {
		item := &d.items[i]
		switch {
		case item.target.IsEmptyBoundary() && item.path == emptyChild:
			empty = item
		case item.target.IsDetachedBoundary() && item.path == staleChild:
			detached = item
		case item.path == staleChild && !item.target.IsBoundary():
			staleMessageRows++
		}
	}
	if empty == nil || detached == nil {
		t.Fatalf("boundaries missing from tree: empty=%v detached=%v items=%+v", empty != nil, detached != nil, d.items)
	}
	if staleMessageRows != 3 {
		t.Fatalf("detached branch rendered %d message rows, want its complete snapshot (3)", staleMessageRows)
	}
	if empty.target.SelectionBoundary != 1 || detached.target.SelectionBoundary != 3 {
		t.Fatalf("boundary indices = empty %+v detached %+v", empty.target, detached.target)
	}
	if !strings.Contains(strings.ToLower(empty.label), "empty") || !strings.Contains(strings.ToLower(detached.label), "detached") {
		t.Fatalf("boundary labels = %q / %q", empty.label, detached.label)
	}
}

func TestSessionTreePreflightIsAtomicOnMalformedFamilyMember(t *testing.T) {
	root := t.TempDir()
	cwd := "/workspace/atomic-tree"
	parent := newTreeTestSession(t, root, cwd, []provider.Message{
		treeTestMessage(provider.RoleUser, provider.TextBlock{Text: "parent"}),
	})
	child, err := core.BranchSessionHidden(parent, root, cwd, "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(child, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not valid json}\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	d := newSessionTreeDialog()
	if !d.OpenMessages([]provider.Message{treeTestMessage(provider.RoleUser, provider.TextBlock{Text: "keep"})}) {
		t.Fatal("failed to seed dialog")
	}
	before := append([]sessionTreeItem(nil), d.items...)
	if d.OpenSessionFamily(root, cwd, parent) {
		t.Fatal("malformed family member passed preflight")
	}
	if !d.Active() || len(d.items) != len(before) || d.items[0].label != before[0].label {
		t.Fatalf("preflight failure changed dialog: active=%v items=%+v", d.Active(), d.items)
	}
}

func TestSanitizeSessionTreeTextPreservesTextAfterNonOSCExit(t *testing.T) {
	input := "before\x1bM and \x1b(B after \x1b]0;window title\a tail"
	if got, want := sanitizeSessionTreeText(input), "beforeM and (B after tail"; got != want {
		t.Fatalf("sanitizeSessionTreeText = %q, want %q", got, want)
	}
}

func TestSessionTreeRenderAndPagingAreWidthAndHeightSafe(t *testing.T) {
	msgs := make([]provider.Message, 0, 20)
	for i := 0; i < 20; i++ {
		msgs = append(msgs, treeTestMessage(provider.RoleUser,
			provider.TextBlock{Text: strings.Repeat("界", 4) + " long row"}))
	}
	d := newSessionTreeDialog()
	d.MaxRows = 2
	if !d.OpenMessages(msgs) {
		t.Fatal("OpenMessages returned false")
	}
	if got := d.HandleKey(tui.Key{Kind: tui.KeyPageUp}); got.Select || d.cursor != 17 {
		t.Fatalf("page up cursor = %d, action=%+v", d.cursor, got)
	}
	for _, width := range []int{0, 1, 7, 17} {
		for _, line := range d.Render(tui.Theme{}, width) {
			if got := sessionTreeANSIWidth(line); got > width {
				t.Fatalf("width %d rendered %d cells: %q", width, got, line)
			}
		}
	}
	if d.viewTop < 0 || d.viewTop+d.MaxRows > len(d.items) {
		t.Fatalf("invalid viewport top=%d rows=%d items=%d", d.viewTop, d.MaxRows, len(d.items))
	}
}

// The checked-out session's context gauge comes from its snapshot, zero
// included, never from the session that was left.
func TestSessionTreeCheckoutRefreshesLastTurnUsageIncludingZero(t *testing.T) {
	root := t.TempDir()
	cwd := "/workspace/tree-usage"
	path := newTreeTestSession(t, root, cwd, []provider.Message{
		treeTestMessage(provider.RoleUser, provider.TextBlock{Text: "question"}),
		treeTestMessage(provider.RoleAssistant, provider.TextBlock{Text: "answer"}),
	})
	target := sessionTreeTarget{
		SourcePath:        path,
		EffectiveIndex:    1,
		SelectionBoundary: 2,
		Role:              provider.RoleAssistant,
	}
	for _, used := range []int{31, 0} {
		i, _, client, _ := newTreePortInteractiveAt(t, root, cwd, path)
		i.mu.Lock()
		i.lastCtxInput = 99
		i.mu.Unlock()
		nextPort := sessionmocks.NewMockPort(t)
		client.EXPECT().Create(mock.Anything, mock.Anything).Return(session.Info{ID: "branch"}, nil).Once()
		client.EXPECT().Attach(mock.Anything, session.ID("branch")).Return(nextPort, nil).Once()
		nextPort.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "branch", CWD: cwd, Run: session.RunStateIdle, ContextUsed: used}}, nil).Once()
		nextPort.EXPECT().Subscribe(mock.Anything, mock.Anything).Return(make(chan session.Event), nil).Once()
		i.applySessionTreeTarget(target, 1)
		awaitSessionSwitch(t, i)
		if i.lastCtxInput != used {
			t.Fatalf("checked-out context usage = %d, want %d", i.lastCtxInput, used)
		}
	}
}

// newTreePortInteractiveAt attaches to an idle session already stored at path.
func newTreePortInteractiveAt(t *testing.T, root, cwd, path string) (*Interactive, *sessionmocks.MockPort, *mockSessionClient, string) {
	t.Helper()
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{
		Info: session.Info{ID: "original", CWD: cwd, SessionPath: path, Run: session.RunStateIdle},
		Seq:  1,
	}, nil).Once()
	port.EXPECT().Subscribe(mock.Anything, uint64(1)).Return(make(chan session.Event), nil).Once()
	client := newMockSessionClient(t)
	i := NewInteractive(InteractiveConfig{Port: port, Client: client, SessionsRoot: root})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	i.runCtx = ctx
	require.NoError(t, i.attachPort(ctx))
	return i, port, client, path
}

// awaitSessionSwitch applies the pending switch result on the test goroutine.
func awaitSessionSwitch(t *testing.T, i *Interactive) {
	t.Helper()
	select {
	case res := <-i.sessionResults:
		require.NoError(t, res.err)
		i.applySessionResult(context.Background(), res)
	case <-time.After(5 * time.Second):
		t.Fatal("session switch did not complete")
	}
}

func treeTestMessage(role provider.Role, content ...provider.Content) provider.Message {
	return provider.Message{Role: role, Content: content, Time: time.Unix(1, 0)}
}

func newTreeTestSession(t *testing.T, root, cwd string, msgs []provider.Message) string {
	t.Helper()
	sess, err := core.NewSession(root, cwd, "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range msgs {
		if err := sess.AppendMessage(msg); err != nil {
			_ = sess.Close()
			t.Fatal(err)
		}
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	return sess.Path
}
