package auth

import (
	"errors"
	"os/exec"
	"runtime"
)

// OpenBrowser starts the system browser without a shell. It returns after
// starting the process and reaps it asynchronously, even for long-lived openers.
func OpenBrowser(raw string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", raw)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", raw)
	default:
		cmd = exec.Command("xdg-open", raw)
	}
	_, err := startBrowser(cmd)
	return err
}

func startBrowser(cmd *exec.Cmd) (<-chan error, error) {
	if err := cmd.Start(); err != nil {
		return nil, errors.New("browser unavailable")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	return done, nil
}
