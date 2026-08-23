package modes

import (
	"testing"

	"github.com/bnema/zut/packages/tui"
)

func TestSettingsDialogDoesNotOfferInheritedTheme(t *testing.T) {
	interactive := NewInteractive(InteractiveConfig{})
	interactive.openSettingsDialog()

	for _, item := range interactive.settingsDialog.items {
		if item.key != "theme" {
			continue
		}
		for _, option := range item.options {
			if option.value == "inherited" {
				t.Fatal("theme picker still offers inherited")
			}
		}
		return
	}
	t.Fatal("settings dialog did not include theme picker")
}

func TestApplyingAutoThemeUsesCapturedTerminalProfile(t *testing.T) {
	profile := tui.TerminalProfile{
		Foreground:    tui.ColorRGB(220, 220, 220),
		Background:    tui.ColorRGB(10, 10, 10),
		HasForeground: true,
		HasBackground: true,
		Depth:         tui.ColorDepthTrueColor,
	}
	interactive := NewInteractive(InteractiveConfig{Theme: tui.TerminalTheme(profile), TerminalProfile: profile, ThemeName: "dark"})
	interactive.rend = nil
	interactive.applyThemeNow("auto")

	if interactive.cfg.Theme.FG != tui.TerminalDefault() {
		t.Fatal("applying auto did not switch to terminal defaults")
	}
	if interactive.cfg.Theme.Terminal != profile {
		t.Fatal("live auto theme lost the captured terminal profile")
	}
}

func TestEnvironmentForcedThemeDoesNotChangeOnSettingsSelection(t *testing.T) {
	interactive := NewInteractive(InteractiveConfig{ThemeForced: true, EffectiveThemeName: "dark", ThemeName: "auto"})
	interactive.rend = nil
	interactive.applyThemeSetting("light")
	if interactive.cfg.ThemeName != "light" {
		t.Fatalf("persisted preference = %q, want light", interactive.cfg.ThemeName)
	}
	if interactive.cfg.EffectiveThemeName != "dark" {
		t.Fatalf("effective preference = %q, want dark", interactive.cfg.EffectiveThemeName)
	}
}
