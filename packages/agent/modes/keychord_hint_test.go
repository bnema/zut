package modes

import "testing"

// TestSlideBackChordHintVSCode pins the VS Code branch: when the
// process is running under VS Code's integrated terminal the hint
// must direct the user to Option+Shift+↑, because plain Option+↑
// gets swallowed by xterm.js's default macOS key handling.
func TestSlideBackChordHintVSCode(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "vscode")
	if got, want := slideBackChordHint(), "Option+Shift+↑"; got != want {
		t.Errorf("VS Code hint = %q; want %q", got, want)
	}
}

// TestSlideBackChordHintVSCodeCaseInsensitive guards against a
// future VS Code change that capitalises the env-var value
// ("VSCode" / "VSCODE"). EqualFold keeps the detection robust.
func TestSlideBackChordHintVSCodeCaseInsensitive(t *testing.T) {
	for _, v := range []string{"VSCode", "VSCODE", "VsCode"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("TERM_PROGRAM", v)
			if got, want := slideBackChordHint(), "Option+Shift+↑"; got != want {
				t.Errorf("TERM_PROGRAM=%q hint = %q; want %q", v, got, want)
			}
		})
	}
}

func TestSlideBackChordHintVev(t *testing.T) {
	for _, value := range []string{"vev", "VEV"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("TERM_PROGRAM", value)
			if got, want := slideBackChordHint(), "Alt+Shift+↑"; got != want {
				t.Errorf("Vev hint = %q; want %q", got, want)
			}
		})
	}
}

// TestSlideBackChordHintDefault pins the default for every
// non-VS-Code terminal: the snappier Option+↑ chord, which Ghostty,
// iTerm2 (with Meta=Option), Terminal.app (with Use Option as Meta),
// Alacritty and Kitty all deliver natively.
func TestSlideBackChordHintDefault(t *testing.T) {
	for _, v := range []string{"", "ghostty", "iTerm.app", "Apple_Terminal", "alacritty", "kitty"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("TERM_PROGRAM", v)
			if got, want := slideBackChordHint(), "Option+↑"; got != want {
				t.Errorf("TERM_PROGRAM=%q hint = %q; want %q", v, got, want)
			}
		})
	}
}
