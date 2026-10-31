package modes

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/provider"
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

func TestResidentSnapshotsFromInfoLiveMetadata(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	summary := session.ResidentSummary{ID: "worker", State: "running", UpdatedAt: now.Add(-time.Minute),
		TurnStartedAt: now.Add(-30 * time.Second), ActivityUpdatedAt: now.Add(-2 * time.Second),
		WaitingForModel: true, Usage: provider.Usage{InputTokens: 1234, OutputTokens: 56},
		ContextUsed: 2048, ContextMax: 8192, Subscription: true}
	data, err := json.Marshal(summary)
	require.NoError(t, err)
	var decoded session.ResidentSummary
	require.NoError(t, json.Unmarshal(data, &decoded))
	rows, total := residentSnapshotsFromInfo([]session.ResidentSummary{decoded}, -1)
	require.Equal(t, 1, total)
	row := rows[0]
	require.Equal(t, summary.TurnStartedAt, row.TurnStartedAt)
	require.Equal(t, summary.ActivityUpdatedAt, row.ActivityUpdatedAt)
	require.True(t, row.WaitingForModel)
	require.Equal(t, summary.Usage, row.Usage)
	require.Equal(t, summary.ContextUsed, row.ContextUsed)
	require.Equal(t, summary.ContextMax, row.ContextMax)
	require.True(t, row.Subscription)
	require.NotEmpty(t, residentUsageMetadata(row))
	require.Equal(t, "2s", formatResidentSubagentActivityAge(activityTime(row), now))
	require.Equal(t, "7s", formatResidentSubagentActivityAge(activityTime(row), now.Add(5*time.Second)))
	require.Equal(t, "30s", formatResidentSubagentActivityAge(turnStartTime(row), now))
	require.Equal(t, "35s", formatResidentSubagentActivityAge(turnStartTime(row), now.Add(5*time.Second)))
}

func TestResidentSnapshotsFromOldDaemon(t *testing.T) {
	var summary session.ResidentSummary
	require.NoError(t, json.Unmarshal([]byte(`{"id":"old","state":"running","updated_at":"2026-10-31T12:00:00Z"}`), &summary))
	rows, _ := residentSnapshotsFromInfo([]session.ResidentSummary{summary}, -1)
	row := rows[0]
	require.True(t, row.TurnStartedAt.IsZero())
	require.True(t, row.ActivityUpdatedAt.IsZero())
	require.False(t, row.WaitingForModel)
	require.Zero(t, row.Usage)
	require.Zero(t, row.ContextMax)
	require.False(t, row.Subscription)
	require.Equal(t, summary.UpdatedAt, activityTime(row))
	require.Equal(t, summary.UpdatedAt, turnStartTime(row))
}
