package agent

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnverifiedDaemonPIDInstruction(t *testing.T) {
	command := "kill -TERM"
	if runtime.GOOS == "windows" {
		command = "taskkill /PID"
	}
	require.EqualError(t, unverifiedDaemonPID(123), "zut daemon: protocol mismatch; could not verify daemon pid 123; stop it manually ("+command+" 123), then retry")
}
