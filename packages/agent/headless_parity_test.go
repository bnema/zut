package agent

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/daemonclient"
	"github.com/stretchr/testify/require"
)

// headlessTestIO points a headless client at an in-process test daemon.
func headlessTestIO(socket string, stdout, stderr *bytes.Buffer, env []string) headlessIO {
	return headlessIO{
		stdout: stdout, stderr: stderr, env: env,
		ensure: daemonclient.EnsureOptions{SocketPath: socket, Options: daemonclient.Options{ClientVersion: "test", ClientKind: "headless"}},
	}
}

func parityEnv() []string { return []string{"PATH=/usr/bin:/bin"} }

// TestHeadlessParityModes runs every recorded print/stream/json scenario
// through the daemon client and compares stdout with the in-process golden.
func TestHeadlessParityModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scenarios use POSIX shell tools")
	}
	for _, sc := range parityScenarios() {
		for _, mode := range sc.modes {
			if mode == "rpc" {
				continue
			}
			t.Run(sc.name+"/"+mode, func(t *testing.T) {
				home := sessionAssemblyEnv(t)
				server := sc.server(t)
				td := startFactoryDaemon(t, home, daemonTestSocket(t))
				cwd := t.TempDir()
				raw := sc.args(mode, server.URL, cwd)
				args, err := ParseArgs(raw)
				require.NoError(t, err)
				var stdout, stderr bytes.Buffer
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				err = runHeadlessClient(ctx, headlessTestIO(td.socket, &stdout, &stderr, parityEnv()), args.Mode, args, raw, "test")
				require.NoError(t, err, "stderr: %s", stderr.String())
				require.Equal(t, readParityGolden(t, sc.name, mode), normaliseParityOutput(stdout.String(), cwd))

				// The session is closed at the end of the run.
				c := dialTestDaemon(t, td.socket)
				list, err := c.List(ctx)
				require.NoError(t, err)
				require.Empty(t, list)
			})
		}
	}
}

// TestHeadlessParityRPC drives the RPC stdio bridge with the recorded
// command scripts and compares stdout with the in-process golden.
func TestHeadlessParityRPC(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scenarios use POSIX shell tools")
	}
	for _, sc := range parityScenarios() {
		if sc.rpcInput == "" {
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			home := sessionAssemblyEnv(t)
			server := sc.server(t)
			td := startFactoryDaemon(t, home, daemonTestSocket(t))
			cwd := t.TempDir()
			raw := sc.args("rpc", server.URL, cwd)
			args, err := ParseArgs(raw)
			require.NoError(t, err)
			var stdout, stderr bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			err = runRPCBridge(ctx, headlessTestIO(td.socket, &stdout, &stderr, parityEnv()), args, raw, "0.0.0-dev", strings.NewReader(sc.rpcInput))
			require.NoError(t, err, "stderr: %s", stderr.String())
			require.Equal(t, readParityGolden(t, sc.name, "rpc"), normaliseParityOutput(stdout.String(), cwd))
			list, err := dialTestDaemon(t, td.socket).List(ctx)
			require.NoError(t, err)
			require.Empty(t, list)
		})
	}
}

// TestHeadlessParityKeepSession leaves the session open with --keep-session
// and keeps stdout identical to the golden.
func TestHeadlessParityKeepSession(t *testing.T) {
	home := sessionAssemblyEnv(t)
	sc := parityScenarios()[0]
	server := sc.server(t)
	td := startFactoryDaemon(t, home, daemonTestSocket(t))
	cwd := t.TempDir()
	raw := append(sc.args("print", server.URL, cwd), "--keep-session")
	args, err := ParseArgs(raw)
	require.NoError(t, err)
	require.True(t, args.KeepSession)
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	require.NoError(t, runHeadlessClient(ctx, headlessTestIO(td.socket, &stdout, &stderr, parityEnv()), args.Mode, args, raw, "test"))
	require.Equal(t, readParityGolden(t, sc.name, "print"), normaliseParityOutput(stdout.String(), cwd))
	list, err := dialTestDaemon(t, td.socket).List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, cwd, list[0].CWD)
}
