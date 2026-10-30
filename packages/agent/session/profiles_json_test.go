package session_test

import (
	"encoding/json"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/stretchr/testify/require"
)

func TestInfoProfilesJSONRoundTrip(t *testing.T) {
	in := session.Info{ID: "s", Profiles: []session.QuickModelShortcut{{Provider: "openai", Model: "gpt-5", Reasoning: "high", FastMode: true}, {}}, ActiveProfile: 1}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	require.Contains(t, string(b), `"profiles":[{"provider":"openai","model":"gpt-5","reasoning":"high","fast_mode":true},{"provider":"","model":""}]`)
	require.Contains(t, string(b), `"active_profile":1`)
	var out session.Info
	require.NoError(t, json.Unmarshal(b, &out))
	require.Equal(t, in.Profiles, out.Profiles)
	require.Equal(t, 1, out.ActiveProfile)
	b, err = json.Marshal(session.Info{})
	require.NoError(t, err)
	require.NotContains(t, string(b), "profiles")
	require.NotContains(t, string(b), "active_profile")
}
