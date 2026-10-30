//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !windows

package daemon

import (
	"path/filepath"
	"testing"
)

func TestListenSocketFailsClosed(t *testing.T) {
	if ln, err := listenSocket(filepath.Join(t.TempDir(), "d.sock"), nil); err == nil {
		ln.close()
		t.Fatal("listenSocket succeeded on an unsupported platform")
	}
}
