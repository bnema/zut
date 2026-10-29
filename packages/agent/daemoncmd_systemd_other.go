//go:build !linux

package agent

import (
	"fmt"
	"io"
)

const systemdUnsupported = `zut daemon: systemd user units are Linux-only.
Start the daemon with "zut daemon start", or add that command as a login item.
`

func daemonInstall(w io.Writer, _ []string) error {
	fmt.Fprint(w, systemdUnsupported)
	return nil
}

func daemonUninstall(w io.Writer, _ []string) error {
	fmt.Fprint(w, systemdUnsupported)
	return nil
}
