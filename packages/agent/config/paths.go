package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// ZutHome returns $ZUT_HOME or the OS-default data dir.
//
// All zut state (config.json, auth.json, sessions/, logs/) lives under
// this directory.
func ZutHome() string {
	if v := os.Getenv("ZUT_HOME"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "zut")
	}
	switch runtime.GOOS {
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "zut")
		}
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "zut")
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "zut")
	}
	return ".zut"
}

// Path returns the path to config.json.
func Path() string { return filepath.Join(ZutHome(), "config.json") }

// AuthPath returns the path to auth.json.
func AuthPath() string { return filepath.Join(ZutHome(), "auth.json") }

// SessionsPath returns the directory holding session files.
func SessionsPath() string { return filepath.Join(ZutHome(), "sessions") }
