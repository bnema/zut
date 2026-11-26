package modes

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionProject(t *testing.T) {
	require.Equal(t, "repo", sessionProject("/a/repo", "/a/other"))
	require.Equal(t, "other", sessionProject("", "/a/other"))
	require.Equal(t, "-", sessionProject("", ""))
	require.NotContains(t, sessionProject("", "/a/bad\x1b[31m"), "\x1b")
}
