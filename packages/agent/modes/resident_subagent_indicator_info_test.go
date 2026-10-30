package modes

import (
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/stretchr/testify/require"
)

func TestResidentSnapshotsFromInfoKeepsActiveOnly(t *testing.T) {
	now := time.Now()
	in := []session.ResidentSummary{
		{ID: "a", Profile: "worker", State: string(subagents.ResidentRunning), UpdatedAt: now},
		{ID: "b", Profile: "worker", State: string(subagents.ResidentCompleted)},
		{ID: "c", Profile: "advisor", State: string(subagents.ResidentQueued)},
		{ID: "d", Profile: "worker", State: string(subagents.ResidentRunning)},
	}
	got, total := residentSnapshotsFromInfo(in, 2)
	require.Equal(t, 3, total)
	require.Len(t, got, 2)
	require.Equal(t, "a", got[0].ID)
	require.Equal(t, subagents.ResidentQueued, got[1].State)
}
