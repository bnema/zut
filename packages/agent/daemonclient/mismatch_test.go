package daemonclient

import (
	"errors"
	"fmt"
	"testing"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/stretchr/testify/require"
)

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
