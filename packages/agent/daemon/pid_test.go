package daemon

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadPID(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(stateDir(home), 0700))
	for _, text := range []string{"", "oops", "0", "-1", "2147483648"} {
		require.NoError(t, os.WriteFile(pidPath(home), []byte(text), 0600))
		_, err := ReadPID(home)
		require.Error(t, err, text)
	}
	require.NoError(t, os.WriteFile(pidPath(home), []byte("123\n"), 0600))
	pid, err := ReadPID(home)
	require.NoError(t, err)
	require.Equal(t, 123, pid)
}
