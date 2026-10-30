package session

import (
	"context"
	"encoding/json"
	"github.com/bnema/zut/packages/agent/tools"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfirmDetachedPersists(t *testing.T) {
	home := t.TempDir()
	store, err := NewFileDeferredStore(home, "test")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewController(ctx, Deps{DeferredStore: store})
	d := c.confirmer().ConfirmToolCall(core.ToolCallConfirmation{Name: "bash", Summary: "echo hi"})
	require.False(t, d.Allow)
	require.Equal(t, detachedActionReason, d.Reason)
	reopened, err := NewFileDeferredStore(home, "test")
	require.NoError(t, err)
	require.Len(t, reopened.List(), 1)
	st, err := os.Stat(filepath.Join(home, "daemon", "sessions", "test", "deferred.jsonl"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), st.Mode().Perm())
	require.NoError(t, c.ResolveDeferred(ctx, reopened.List()[0].ID, "dismiss"))
	require.Empty(t, store.List())
}
func TestConfirmPendingDecisions(t *testing.T) {
	for _, mode := range []string{"allow", "deny", "detach", "abort"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := NewController(ctx, Deps{})
			require.NoError(t, c.call(ctx, func() { c.attach = AttachStateAttached }))
			events, err := c.Subscribe(ctx, 0)
			require.NoError(t, err)
			result := make(chan core.ConfirmDecision, 1)
			go func() { result <- c.confirmer().ConfirmToolCall(core.ToolCallConfirmation{Name: "bash"}) }()
			ev := <-events
			require.Equal(t, EvPending, ev.Kind)
			id := ev.Data.(PendingEvent).Pending.ID
			switch mode {
			case "allow", "deny":
				require.NoError(t, c.AnswerConfirmation(ctx, id, core.ConfirmDecision{Allow: mode == "allow", RememberAll: mode == "allow"}))
			case "detach":
				require.NoError(t, c.call(ctx, func() { c.attach = AttachStateDetached; c.denyAllPending("detached") }))
			case "abort":
				require.NoError(t, c.call(ctx, func() { c.denyAllPending("aborted") }))
			}
			d := <-result
			require.Equal(t, mode == "allow", d.Allow)
			require.Equal(t, mode == "allow", d.RememberAll)
			snap, err := c.Snapshot(ctx)
			require.NoError(t, err)
			require.Empty(t, snap.Pending)
			if mode == "detach" {
				require.Len(t, snap.Deferred, 1)
			} else {
				require.Empty(t, snap.Deferred)
			}
		})
	}
}
func TestConfirmAwaitPendingDetached(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewController(ctx, Deps{})
	_, err := c.awaitPending(ctx, PendingRequest{Kind: "rescue"})
	require.ErrorIs(t, err, ErrNotAttached)
}

func TestConfirmQuestionsRescueConcurrent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewController(ctx, Deps{})
	require.NoError(t, c.call(ctx, func() { c.attach = AttachStateAttached }))
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	const count = 20
	results := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() {
			_, err := c.awaitPending(ctx, PendingRequest{Kind: "rescue", Rescue: &RescueOptions{Candidates: []string{"p/m"}}})
			results <- err
		}()
	}
	for n := 0; n < count; {
		ev := <-events
		if ev.Kind != EvPending {
			continue
		}
		p := ev.Data.(PendingEvent).Pending
		require.NoError(t, c.AnswerQuestion(ctx, p.ID, []tools.QuestionAnswer{{Selected: []string{"p/m"}}}))
		n++
	}
	for i := 0; i < count; i++ {
		require.NoError(t, <-results)
	}
	qctx, qcancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := c.questionAsker().AskQuestions(qctx, nil); done <- err }()
	for (<-events).Kind != EvPending {
	}
	qcancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.NoError(t, c.call(ctx, func() { c.attach = AttachStateDetached }))
	_, err = c.questionAsker().AskQuestions(ctx, nil)
	require.EqualError(t, err, detachedQuestionReason)
}
func TestConfirmRestoredOwnershipAndSendBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := NewFileDeferredStore(t.TempDir(), "restore")
	require.NoError(t, err)
	require.NoError(t, store.Append(DeferredAction{ID: "old", Tool: "bash", Summary: "hi", Questions: []tools.Question{{Question: "original"}}}))
	c := NewController(ctx, Deps{DeferredStore: store})
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snap.Deferred, 1)
	snap.Deferred[0].Questions[0].Question = "changed"
	snap, err = c.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, "original", snap.Deferred[0].Questions[0].Question)
	c.confirmer().ConfirmToolCall(core.ToolCallConfirmation{Name: "bash"})
	require.NotEqual(t, "old", store.List()[1].ID)
	turn := newMockturnSeam(t)
	turn.EXPECT().turnInfo().Return(turnInfoProjection{})
	turn.EXPECT().isBusy().Return(false)
	turn.EXPECT().turnOrigin().Return(InputOrigin("")).Maybe()
	turn.EXPECT().startTurn(Input{Origin: InputOriginUser, Text: "The user reviewed a previously denied action and approves it: bash — hi. Retry it if still relevant."}).Return(nil).Once()
	require.NoError(t, c.call(ctx, func() { c.turnSeam = turn }))
	require.NoError(t, c.ResolveDeferred(ctx, "old", "send_to_agent"))
}

// ConfirmTestSetup exposes the private cluster only to external generated-mock tests.
func ConfirmTestSetup(ctx context.Context, deps Deps, attached bool) (*Controller, core.Confirmer) {
	c := NewController(ctx, deps)
	if attached {
		_ = c.call(ctx, func() { c.attach = AttachStateAttached })
	}
	return c, c.confirmer().(core.Confirmer)
}

func TestConfirmQuestionAnswerAndDetach(t *testing.T) {
	for _, detach := range []bool{false, true} {
		t.Run(strconv.FormatBool(detach), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := NewController(ctx, Deps{})
			require.NoError(t, c.call(ctx, func() { c.attach = AttachStateAttached }))
			events, err := c.Subscribe(ctx, 0)
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() {
				_, err := c.questionAsker().AskQuestions(ctx, []tools.Question{{Question: "choose"}})
				done <- err
			}()
			p := (<-events).Data.(PendingEvent).Pending
			snap, err := c.Snapshot(ctx)
			require.NoError(t, err)
			snap.Pending[0].Questions[0].Question = "mutated"
			snap, err = c.Snapshot(ctx)
			require.NoError(t, err)
			require.Equal(t, "choose", snap.Pending[0].Questions[0].Question)
			if detach {
				require.NoError(t, c.call(ctx, func() { c.attach = AttachStateDetached; c.denyAllPending("detached") }))
				require.EqualError(t, <-done, detachedQuestionReason)
			} else {
				require.NoError(t, c.AnswerQuestion(ctx, p.ID, []tools.QuestionAnswer{{Custom: "x"}}))
				require.NoError(t, <-done)
			}
		})
	}
}

func TestConfirmDuplicateCallerIDs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewController(ctx, Deps{})
	require.NoError(t, c.call(ctx, func() {
		a := c.registerPending(PendingRequest{ID: "duplicate", Kind: "rescue"})
		b := c.registerPending(PendingRequest{ID: "duplicate", Kind: "rescue"})
		require.NotEqual(t, "duplicate", a.request.ID)
		require.NotEqual(t, a.request.ID, b.request.ID)
		require.Len(t, c.confirm.pending, 2)
		c.denyAllPending("aborted")
		require.Error(t, (<-a.result).err)
		require.Error(t, (<-b.result).err)
	}))
}
func TestConfirmCancelRegistrationRace(t *testing.T) {
	for i := 0; i < 30; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		c := NewController(ctx, Deps{})
		require.NoError(t, c.call(ctx, func() { c.attach = AttachStateAttached }))
		reqctx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { _, err := c.awaitPending(reqctx, PendingRequest{Kind: "rescue"}); done <- err }()
		stop()
		require.NoError(t, c.call(ctx, func() { c.denyAllPending("aborted") }))
		require.Error(t, <-done)
		snap, err := c.Snapshot(ctx)
		require.NoError(t, err)
		require.Empty(t, snap.Pending)
		cancel()
	}
}

func ConfirmTestDispatch(t *testing.T, c *Controller) {
	turn := newMockturnSeam(t)
	turn.EXPECT().isBusy().Return(false)
	turn.EXPECT().turnOrigin().Return(InputOrigin(""))
	turn.EXPECT().turnInfo().Return(turnInfoProjection{})
	turn.EXPECT().startTurn(Input{Origin: InputOriginUser, Text: "The user reviewed a previously denied action and approves it: bash — hi. Retry it if still relevant."}).Return(nil).Once()
	require.NoError(t, c.call(context.Background(), func() { c.turnSeam = turn }))
}

func TestConfirmNilAnswerDeclinesQuestion(t *testing.T) {
	c, ctx := newTestController(t)
	require.NoError(t, c.call(ctx, func() { c.attach = AttachStateAttached }))
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	tool := &tools.AskUserQuestionTool{Asker: c.questionAsker}
	done := make(chan core.ToolResult, 1)
	go func() {
		res, err := tool.Execute(ctx, json.RawMessage(`{"questions":[{"question":"choose"}]}`), nil)
		assert.NoError(t, err)
		done <- res
	}()
	p := (<-events).Data.(PendingEvent).Pending
	require.NoError(t, c.AnswerQuestion(ctx, p.ID, nil))
	res := <-done
	require.False(t, res.IsError)
	require.Equal(t, []provider.Content{provider.TextBlock{Text: "The user declined to answer the questions."}}, res.Content)
}
