package daemonproto

import (
	"encoding/json"
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/stretchr/testify/require"
)

func TestResidentMessagesMissingFields(t *testing.T) {
	for _, raw := range []string{`{}`, `{"future_field":true}`} {
		var list ResidentListParams
		var history ResidentHistoryParams
		var data ResidentListData
		var page subagents.ResidentHistoryPage
		require.NoError(t, json.Unmarshal([]byte(raw), &list))
		require.NoError(t, json.Unmarshal([]byte(raw), &history))
		require.NoError(t, json.Unmarshal([]byte(raw), &data))
		require.NoError(t, json.Unmarshal([]byte(raw), &page))
		require.Zero(t, list)
		require.Zero(t, history)
		require.Zero(t, data)
		require.Zero(t, page)
	}
}
