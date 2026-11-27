package config

import (
	"path/filepath"
	"testing"
)

func TestZutHomePrefersExplicitHomeOverXDGStateHome(t *testing.T) {
	t.Setenv("ZUT_HOME", filepath.Join("explicit", "zut"))
	t.Setenv("XDG_STATE_HOME", filepath.Join("xdg", "state"))

	if got, want := ZutHome(), filepath.Join("explicit", "zut"); got != want {
		t.Fatalf("ZutHome() = %q, want %q", got, want)
	}
}

func TestZutHomeUsesXDGStateHome(t *testing.T) {
	t.Setenv("ZUT_HOME", "")
	t.Setenv("XDG_STATE_HOME", filepath.Join("xdg", "state"))

	if got, want := ZutHome(), filepath.Join("xdg", "state", "zut"); got != want {
		t.Fatalf("ZutHome() = %q, want %q", got, want)
	}
}
