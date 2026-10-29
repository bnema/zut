//go:build linux

package agent

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	systemdUnitName      = "zut-daemon"
	systemdInstallHint   = "systemctl --user daemon-reload && systemctl --user enable --now zut-daemon"
	systemdUninstallHint = "systemctl --user disable --now zut-daemon && systemctl --user daemon-reload"
)

// systemctlRun runs `systemctl <args>` with stdout/stderr passed through.
// Tests replace it so they never invoke systemctl.
var systemctlRun = func(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// systemdExecutable resolves the binary written to ExecStart. Tests replace it.
var systemdExecutable = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(exe)
}

func systemdUnitPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user", systemdUnitName+".service"), nil
}

func systemdUnit(exe string) string {
	return "[Unit]\nDescription=zut daemon\n\n[Service]\nExecStart=" + exe + " daemon run\nRestart=on-failure\n\n[Install]\nWantedBy=default.target\n"
}

func parseNowFlag(name string, args []string) (bool, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	now := fs.Bool("now", false, "run the systemctl commands instead of printing them")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	if fs.NArg() > 0 {
		return false, fmt.Errorf("%s: unexpected arguments %v", name, fs.Args())
	}
	return *now, nil
}

func daemonInstall(w io.Writer, args []string) error {
	now, err := parseNowFlag("zut daemon install", args)
	if err != nil {
		return err
	}
	exe, err := systemdExecutable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	path, err := systemdUnitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(systemdUnit(exe)), 0o644); err != nil {
		return err
	}
	// WriteFile keeps the mode of an existing file; enforce 0644.
	if err := os.Chmod(path, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(w, "zut daemon: wrote %s\n", path)
	if !now {
		fmt.Fprintln(w, systemdInstallHint)
		return nil
	}
	if err := systemctlRun("--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if err := systemctlRun("--user", "enable", "--now", systemdUnitName); err != nil {
		return fmt.Errorf("systemctl enable: %w", err)
	}
	return nil
}

func daemonUninstall(w io.Writer, args []string) error {
	now, err := parseNowFlag("zut daemon uninstall", args)
	if err != nil {
		return err
	}
	path, err := systemdUnitPath()
	if err != nil {
		return err
	}
	if now {
		// Disable before removing the unit, so systemd still knows it.
		if err := systemctlRun("--user", "disable", "--now", systemdUnitName); err != nil {
			return fmt.Errorf("systemctl disable: %w", err)
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Fprintf(w, "zut daemon: removed %s\n", path)
	if !now {
		fmt.Fprintln(w, systemdUninstallHint)
		return nil
	}
	if err := systemctlRun("--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	return nil
}
