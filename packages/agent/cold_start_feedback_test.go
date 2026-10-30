package agent

import (
	"bytes"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/daemonclient"
	"github.com/stretchr/testify/require"
)

func TestColdStartFeedbackPrintsToWriter(t *testing.T) {
	var out bytes.Buffer
	opts := withColdStartFeedback(&out, daemonclient.EnsureOptions{})
	require.Empty(t, out.String(), "nothing is printed until a daemon is spawned")
	opts.OnSpawn()
	opts.OnReady(1840 * time.Millisecond)
	require.Equal(t, "Starting zut daemon…\nzut daemon ready (1.8s)\n", out.String())
}
