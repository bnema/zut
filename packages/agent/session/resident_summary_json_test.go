package session_test

import (
	"encoding/json"
	"testing"
	"time"

	. "github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/require"
)

func TestResidentSummaryJSONLiveMetadata(t *testing.T) {
	zero, err := json.Marshal(ResidentSummary{ID: "child"})
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(zero, &fields))
	for _, key := range []string{"turn_started_at", "activity_updated_at", "waiting_for_model", "usage", "context_used", "context_max", "subscription"} {
		require.NotContains(t, fields, key)
	}

	at := time.Date(2026, time.October, 31, 12, 0, 0, 0, time.UTC)
	live := ResidentSummary{ID: "child", TurnStartedAt: at, ActivityUpdatedAt: at.Add(time.Second),
		WaitingForModel: true, Usage: provider.Usage{InputTokens: 12, OutputTokens: 3},
		ContextUsed: 40, ContextMax: 100, Subscription: true}
	data, err := json.Marshal(live)
	require.NoError(t, err)
	var decoded ResidentSummary
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, live, decoded)
}
