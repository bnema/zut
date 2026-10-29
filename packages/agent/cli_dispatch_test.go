package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/stretchr/testify/require"
)

func TestDaemonClientSubcommandRoutes(t *testing.T) {
	cases := map[string]any{
		"ls":     runLsCommand,
		"tree":   runTreeCommand,
		"kill":   runKillCommand,
		"attach": runAttachCommand,
	}
	for name, fn := range cases {
		got := daemonClientSubcommand(name)
		require.NotNil(t, got, name)
		require.Equal(t, reflect.ValueOf(fn).Pointer(), reflect.ValueOf(got).Pointer(), name)
	}
	for _, name := range []string{"", "daemon", "sessions", "rpc", "lsx", "--ls"} {
		require.Nil(t, daemonClientSubcommand(name), name)
	}
}

func testLocator(t *testing.T) (sessionLocator, *[]string) {
	calls := &[]string{}
	return sessionLocator{
		latest: func(root, cwd string) string { *calls = append(*calls, "latest:"+cwd); return "/s/latest.jsonl" },
		findByID: func(_ context.Context, id string) (string, error) {
			*calls = append(*calls, "find:"+id)
			if id == "00000000-0000-0000-0000-000000000002" {
				return "", nil
			}
			return "/s/" + id + ".jsonl", nil
		},
		prune: func(root, cwd string) { *calls = append(*calls, "prune:"+cwd) },
		pick: func(root, cwd string) (string, error) {
			*calls = append(*calls, "pick:"+cwd)
			return "/s/picked.jsonl", nil
		},
	}, calls
}

func TestBuildCreateSpec(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	const id = "00000000-0000-0000-0000-000000000001"
	env := []string{"A=1"}
	tests := []struct {
		name  string
		raw   []string
		want  string
		calls []string
	}{
		{"fresh", []string{"hello"}, "", []string{"prune:/w"}},
		{"continue", []string{"-c"}, "/s/latest.jsonl", []string{"prune:/w", "latest:/w"}},
		{"resume id", []string{"--resume", id}, "/s/" + id + ".jsonl", []string{"find:" + id}},
		{"bare resume picks", []string{"--resume"}, "/s/picked.jsonl", []string{"prune:/w", "pick:/w"}},
		{"session path", []string{"--session", "/x/s.jsonl"}, "/x/s.jsonl", nil},
		{"no session", []string{"--no-session", "-c"}, "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args, err := ParseArgs(append(tc.raw, "--cwd", "/w"))
			require.NoError(t, err)
			loc, calls := testLocator(t)
			spec, err := buildCreateSpec(context.Background(), args, tc.raw, "/proc/cwd", env, loc)
			require.NoError(t, err)
			require.Equal(t, &session.CreateSpec{CWD: "/proc/cwd", Env: env, Args: tc.raw, ResumePath: tc.want}, spec)
			require.Equal(t, tc.calls, nilIfEmpty(*calls))
		})
	}
}

func TestBuildCreateSpecErrors(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	args, err := ParseArgs([]string{"--resume", "00000000-0000-0000-0000-000000000002"})
	require.NoError(t, err)
	loc, _ := testLocator(t)
	_, err = buildCreateSpec(context.Background(), args, nil, "/w", nil, loc)
	require.ErrorContains(t, err, "not found")

	boom := errors.New("boom")
	args, err = ParseArgs([]string{"--resume"})
	require.NoError(t, err)
	loc.pick = func(string, string) (string, error) { return "", boom }
	_, err = buildCreateSpec(context.Background(), args, nil, "/w", nil, loc)
	require.ErrorIs(t, err, boom)
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func TestBuildCreateSpecEmptyPickStartsFresh(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	raw := []string{"--model", "x", "--resume"}
	args, err := ParseArgs(raw)
	require.NoError(t, err)
	loc, _ := testLocator(t)
	loc.pick = func(string, string) (string, error) { return "", nil }
	spec, err := buildCreateSpec(context.Background(), args, raw, "/w", nil, loc)
	require.NoError(t, err)
	require.Empty(t, spec.ResumePath)
	require.Equal(t, []string{"--model", "x"}, spec.Args)
}
