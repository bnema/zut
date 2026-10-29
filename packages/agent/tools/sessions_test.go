package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func sessionsResult(t *testing.T, tool *SessionsTool, args string) (string, bool) {
	t.Helper()
	res, err := tool.Execute(context.Background(), json.RawMessage(args), nil)
	require.NoError(t, err)
	var out strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(provider.TextBlock); ok {
			out.WriteString(text.Text)
		}
	}
	return out.String(), res.IsError
}

var sessionsTestEntries = []SessionEntry{
	{ID: "abc123", Title: "api work", CWD: "/repo/a", Attach: "attached", Run: "idle", Activity: "idle", Model: "gpt-5"},
	{ID: "abd456", Title: "docs", CWD: "/repo/b", Attach: "detached", Run: "running", Activity: "tool", Model: "gpt-5"},
	{ID: "xyz789", Title: "docs", CWD: "/repo/c", Attach: "detached", Run: "idle"},
	{ID: "qqq000", Title: "abc", CWD: "/repo/d"},
}

func TestSessionsToolMetadata(t *testing.T) {
	tool := &SessionsTool{}
	require.Equal(t, SessionsToolName, tool.Name())
	require.True(t, json.Valid(tool.Schema()))
	desc := tool.Description()
	for _, want := range []string{"attached", "another session's message", "workspace", "sub-agents", "plan step", "goal", "unique ID prefix", "exact title"} {
		require.Contains(t, desc, want)
	}
	var _ core.Tool = tool
}

func TestSessionsToolList(t *testing.T) {
	dir := newMockSessionDirectory(t)
	dir.EXPECT().List(mock.Anything).Return(sessionsTestEntries[:3], nil).Once()
	out, isErr := sessionsResult(t, &SessionsTool{Directory: dir}, `{"action":"list"}`)
	require.False(t, isErr)
	require.Equal(t, "abc123  attached  idle  idle  gpt-5  /repo/a  api work\n"+
		"abd456  detached  running  tool  gpt-5  /repo/b  docs\n"+
		"xyz789  detached  idle  -  -  /repo/c  docs", out)
}

func TestSessionsToolListEmptyAndError(t *testing.T) {
	dir := newMockSessionDirectory(t)
	dir.EXPECT().List(mock.Anything).Return(nil, nil).Once()
	out, isErr := sessionsResult(t, &SessionsTool{Directory: dir}, `{"action":"list"}`)
	require.False(t, isErr)
	require.Equal(t, "no open sessions", out)

	dir.EXPECT().List(mock.Anything).Return(nil, errors.New("boom")).Once()
	out, isErr = sessionsResult(t, &SessionsTool{Directory: dir}, `{"action":"list"}`)
	require.True(t, isErr)
	require.Equal(t, "sessions: boom", out)
}

func TestSessionsToolTree(t *testing.T) {
	dir := newMockSessionDirectory(t)
	dir.EXPECT().Tree(mock.Anything).Return("/repo\n  abc123 api work", nil).Once()
	out, isErr := sessionsResult(t, &SessionsTool{Directory: dir}, `{"action":"tree"}`)
	require.False(t, isErr)
	require.Equal(t, "/repo\n  abc123 api work", out)

	dir.EXPECT().Tree(mock.Anything).Return("", nil).Once()
	out, _ = sessionsResult(t, &SessionsTool{Directory: dir}, `{"action":"tree"}`)
	require.Equal(t, "no open sessions", out)
}

func TestSessionsToolSendResolution(t *testing.T) {
	cases := []struct {
		to, want string
	}{
		{"abc123", "abc123"}, // exact ID
		{"abd", "abd456"},    // unique prefix
		{"api work", "abc123"},
		{"abc", "abc123"}, // an ID prefix wins over an exact title
	}
	for _, tc := range cases {
		t.Run(tc.to, func(t *testing.T) {
			dir := newMockSessionDirectory(t)
			dir.EXPECT().List(mock.Anything).Return(sessionsTestEntries, nil).Once()
			dir.EXPECT().Send(mock.Anything, tc.want, "hello").Return(nil).Once()
			args, _ := json.Marshal(map[string]string{"action": "send", "to": tc.to, "text": "hello"})
			out, isErr := sessionsResult(t, &SessionsTool{Directory: dir}, string(args))
			require.False(t, isErr, out)
			require.Equal(t, "message sent to session "+tc.want, out)
		})
	}
}

func TestSessionsToolSendResolutionErrors(t *testing.T) {
	cases := []struct {
		to, want string
	}{
		{"ab", `sessions: session prefix "ab" is ambiguous: abc123, abd456`},
		{"docs", `sessions: session title "docs" is ambiguous: abd456, xyz789`},
		{"nope", `sessions: no open session matches "nope"`},
	}
	for _, tc := range cases {
		t.Run(tc.to, func(t *testing.T) {
			dir := newMockSessionDirectory(t)
			dir.EXPECT().List(mock.Anything).Return(sessionsTestEntries, nil).Once()
			args, _ := json.Marshal(map[string]string{"action": "send", "to": tc.to, "text": "hello"})
			out, isErr := sessionsResult(t, &SessionsTool{Directory: dir}, string(args))
			require.True(t, isErr)
			require.Equal(t, tc.want, out)
		})
	}
}

func TestSessionsToolSendErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ErrSenderDetached, "sending requires an attached session (the user must be present)"},
		{fmt.Errorf("wrapped: %w", ErrSenderLoop), "cannot send from a turn started by another session's message"},
		{errors.New("queue full"), "sessions: send: queue full"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			dir := newMockSessionDirectory(t)
			dir.EXPECT().List(mock.Anything).Return(sessionsTestEntries, nil).Once()
			dir.EXPECT().Send(mock.Anything, "abc123", "hi").Return(tc.err).Once()
			out, isErr := sessionsResult(t, &SessionsTool{Directory: dir}, `{"action":"send","to":"abc123","text":"hi"}`)
			require.True(t, isErr)
			require.Equal(t, tc.want, out)
		})
	}
}

func TestSessionsToolInvalidArgs(t *testing.T) {
	dir := newMockSessionDirectory(t)
	tool := &SessionsTool{Directory: dir}
	for args, want := range map[string]string{
		`{}`:                              "sessions: action is required",
		`{"action":"kill"}`:               "sessions: unknown action",
		`{"action":"list","extra":1}`:     "sessions: invalid arguments",
		`{"action":"list","to":"abc"}`:    "sessions: to and text are only valid for action send",
		`{"action":"tree","text":"x"}`:    "sessions: to and text are only valid for action send",
		`{"action":"send","text":"x"}`:    "sessions: to is required for send",
		`{"action":"send","to":"abc123"}`: "sessions: text is required for send",
		`not json`:                        "sessions: invalid arguments",
	} {
		out, isErr := sessionsResult(t, tool, args)
		require.True(t, isErr, args)
		require.Equal(t, want, out, args)
	}
	out, isErr := sessionsResult(t, &SessionsTool{}, `{"action":"list"}`)
	require.True(t, isErr)
	require.Equal(t, "sessions: directory is unavailable", out)
}
