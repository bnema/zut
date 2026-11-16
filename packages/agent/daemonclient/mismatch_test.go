package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/stretchr/testify/require"
)

func TestRetryStartupDialTimeoutBeforeContextCancellation(t *testing.T) {
	// net.Dialer can return its own deadline error before ctx.Err is set.
	ctx := context.Background()
	err := fmt.Errorf("dial zut daemon: %w", &net.OpError{Op: "dial", Net: "unix", Err: os.ErrDeadlineExceeded})
	require.NoError(t, ctx.Err())
	require.True(t, retryStartupDial(err), "timeouts must reach startup log-tail reporting")
	require.False(t, retryStartupDial(fmt.Errorf("hello: %w", ErrProtocolMismatch)))
	require.False(t, retryStartupDial(os.ErrPermission))
}

func TestClassifyHelloError(t *testing.T) {
	for _, tc := range []struct {
		err      error
		mismatch bool
	}{
		{&daemonproto.RemoteError{Msg: "protocol version 1 unsupported (daemon speaks 2)"}, true},
		{fmt.Errorf("wrapped: %w", &daemonproto.RemoteError{Msg: "protocol version 2 unsupported (daemon speaks 1)"}), true},
		{errors.New("protocol version 1 unsupported (daemon speaks 2)"), false},
		{&daemonproto.RemoteError{Msg: "other protocol error"}, false},
		{&daemonproto.RemoteError{Msg: "protocol version 1 unsupported (daemon speaks 2) extra"}, false},
	} {
		got := classifyHelloError(tc.err)
		require.Equal(t, tc.mismatch, errors.Is(got, ErrProtocolMismatch))
		if tc.mismatch {
			require.Contains(t, got.Error(), `run "zut daemon restart"`)
		}
	}
}
