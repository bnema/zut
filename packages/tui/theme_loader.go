package tui

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/styles"
)

// ThemeOption is one selectable theme discovered under $ZUT_HOME/themes.
// Value is stored in config.json.
type ThemeOption struct {
	Value       string
	Label       string
	Description string
	Path        string
	Builtin     bool
}

// ThemeFile is the user-editable JSON shape loaded from
// $ZUT_HOME/themes/*.json. It carries metadata plus separate overrides
// for dark and light terminals.
type ThemeFile struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Colors      ThemeFileColorModes `json:"colors"`
	Overrides   ThemeOverrides      `json:"-"`
}

func (tf *ThemeFile) UnmarshalJSON(data []byte) error {
	type alias ThemeFile
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	// Allow a tiny theme file with overrides at the top level, e.g.
	// {"spinner_frames":[".","o"],"spinner_interval_ms":120}.
	// Metadata fields are ignored by ThemeOverrides because they do not
	// have matching json tags.
	if err := json.Unmarshal(data, &a.Overrides); err != nil {
		return fmt.Errorf("theme overrides: %w", err)
	}
	*tf = ThemeFile(a)
	return validateThemeFile(*tf)
}

type ThemeFileColorModes struct {
	Base     ThemeOverrides `json:"-"`
	Dark     ThemeOverrides `json:"dark"`
	Light    ThemeOverrides `json:"light"`
	HasDark  bool           `json:"-"`
	HasLight bool           `json:"-"`
}

func (m *ThemeFileColorModes) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if v, ok := raw["dark"]; ok {
		m.HasDark = true
		if err := json.Unmarshal(v, &m.Dark); err != nil {
			return fmt.Errorf("dark overrides: %w", err)
		}
	}
	if v, ok := raw["light"]; ok {
		m.HasLight = true
		if err := json.Unmarshal(v, &m.Light); err != nil {
			return fmt.Errorf("light overrides: %w", err)
		}
	}
	// Also allow colors to directly contain overrides shared by both
	// modes, e.g. {"colors":{"accent":204}}.
	var base ThemeOverrides
	if err := json.Unmarshal(data, &base); err != nil {
		return fmt.Errorf("shared overrides: %w", err)
	}
	m.Base = base
	return nil
}

// ThemeOverrides is intentionally pointer-based so a theme file can
// override only the colors it cares about and inherit the built-in
// dark/light defaults for everything else.
type ThemeOverrides struct {
	FG                *TerminalColorValue  `json:"fg,omitempty"`
	Muted             *TerminalColorValue  `json:"muted,omitempty"`
	Accent            *TerminalColorValue  `json:"accent,omitempty"`
	Background        *TerminalColorValue  `json:"background,omitempty"`
	User              *TerminalColorValue  `json:"user,omitempty"`
	UserBubbleBG      *TerminalColorValue  `json:"user_bubble_bg,omitempty"`
	UserBubbleFG      *TerminalColorValue  `json:"user_bubble_fg,omitempty"`
	Assistant         *TerminalColorValue  `json:"assistant,omitempty"`
	Tool              *TerminalColorValue  `json:"tool,omitempty"`
	ToolOut           *TerminalColorValue  `json:"tool_out,omitempty"`
	Error             *TerminalColorValue  `json:"error,omitempty"`
	Warning           *TerminalColorValue  `json:"warning,omitempty"`
	Spinner           *TerminalColorValue  `json:"spinner,omitempty"`
	ThinkingMax       *TerminalColorValue  `json:"thinking_max,omitempty"`
	ThinkingMaxCamel  *TerminalColorValue  `json:"thinkingMax,omitempty"`
	SelectionBG       *TerminalColorValue  `json:"selection_bg,omitempty"`
	SelectionFG       *TerminalColorValue  `json:"selection_fg,omitempty"`
	SpinnerFrames     []string             `json:"spinner_frames,omitempty"`
	SpinnerIntervalMS *int                 `json:"spinner_interval_ms,omitempty"`
	SyntaxBaseStyle   *string              `json:"syntax_base_style,omitempty"`
	Syntax            SyntaxThemeOverrides `json:"syntax,omitempty"`
}

type SyntaxThemeOverrides struct {
	Keyword             *string `json:"keyword,omitempty"`
	KeywordConstant     *string `json:"keyword_constant,omitempty"`
	KeywordDeclaration  *string `json:"keyword_declaration,omitempty"`
	KeywordNamespace    *string `json:"keyword_namespace,omitempty"`
	KeywordReserved     *string `json:"keyword_reserved,omitempty"`
	KeywordType         *string `json:"keyword_type,omitempty"`
	NameBuiltin         *string `json:"name_builtin,omitempty"`
	NameFunction        *string `json:"name_function,omitempty"`
	NameClass           *string `json:"name_class,omitempty"`
	NameDecorator       *string `json:"name_decorator,omitempty"`
	LiteralString       *string `json:"literal_string,omitempty"`
	LiteralStringEscape *string `json:"literal_string_escape,omitempty"`
	LiteralNumber       *string `json:"literal_number,omitempty"`
	Comment             *string `json:"comment,omitempty"`
	CommentPreproc      *string `json:"comment_preproc,omitempty"`
	Operator            *string `json:"operator,omitempty"`
	Punctuation         *string `json:"punctuation,omitempty"`
	Text                *string `json:"text,omitempty"`
}

// TerminalColorValue accepts any of these JSON forms:
//
//	24                         // xterm-256 color index
//	"#42454b"                  // RGB hex
//	{"mode":"ansi","index":100}
//	{"mode":"rgb","r":66,"g":69,"b":75}
//	{"mode":"256","index":254}
type TerminalColorValue struct {
	TerminalColor
}

func (c *TerminalColorValue) UnmarshalJSON(data []byte) error {
	var index int
	if err := json.Unmarshal(data, &index); err == nil {
		c.TerminalColor = Color256(index)
		return validateExplicitTerminalColor(c.TerminalColor)
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if rgb, ok := parseHexColor(s); ok {
			c.TerminalColor = rgb
			return validateExplicitTerminalColor(c.TerminalColor)
		}
		return fmt.Errorf("invalid terminal color %q", s)
	}
	var obj struct {
		Mode  string `json:"mode"`
		Index int    `json:"index"`
		R     int    `json:"r"`
		G     int    `json:"g"`
		B     int    `json:"b"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	switch strings.ToLower(obj.Mode) {
	case "", "256", "color256", "xterm256":
		c.TerminalColor = Color256(obj.Index)
	case "ansi":
		c.TerminalColor = ColorANSI(obj.Index)
	case "rgb", "truecolor":
		c.TerminalColor = ColorRGB(obj.R, obj.G, obj.B)
	default:
		return fmt.Errorf("unknown terminal color mode %q", obj.Mode)
	}
	return validateExplicitTerminalColor(c.TerminalColor)
}

// TerminalTheme constructs zut's adaptive default from terminal-owned
// defaults and ANSI slots. It never chooses a fixed built-in palette merely
// because a terminal reports a light background.
func TerminalTheme(profile TerminalProfile) Theme {
	t := Theme{
		FG:                 TerminalDefault(),
		Muted:              TerminalPaletteSlot(8),
		Accent:             TerminalPaletteSlot(12),
		User:               TerminalPaletteSlot(13),
		UserBubbleBG:       TerminalPaletteSlot(8),
		UserBubbleFG:       TerminalDefault(),
		Assistant:          TerminalPaletteSlot(12),
		Tool:               TerminalPaletteSlot(10),
		ToolOut:            TerminalPaletteSlot(8),
		Error:              TerminalPaletteSlot(9),
		Warning:            TerminalPaletteSlot(11),
		Spinner:            TerminalPaletteSlot(13),
		ThinkingMax:        TerminalPaletteSlot(13),
		SelectionBG:        TerminalPaletteSlot(4),
		SelectionFG:        TerminalDefault(),
		UseTerminalPalette: true,
		Terminal:           profile,
		SpinnerFrames:      append([]string(nil), defaultSpinnerFrames...),
		SpinnerIntervalMS:  80,
		SyntaxBaseStyle:    "",
		Syntax:             terminalSyntaxTheme(),
	}
	if profile.HasForeground && profile.HasBackground {
		t.Muted = t.DimColor(profile.Foreground, 45)
		t.ToolOut = t.Muted
		t.UserBubbleBG = blendTerminalColors(profile.Background, profile.Foreground, 12)
		t.SelectionBG = blendTerminalColors(profile.Background, profile.Foreground, 25)
	}
	// Auto must leave both the terminal background and cursor color owned by
	// the terminal. Custom themes can intentionally override Background.
	t.Background = nil
	return t
}

func terminalSyntaxTheme() SyntaxTheme {
	return SyntaxTheme{
		Keyword:             "ansi:12 bold",
		KeywordConstant:     "ansi:12",
		KeywordDeclaration:  "ansi:12",
		KeywordNamespace:    "ansi:12",
		KeywordReserved:     "ansi:12 bold",
		KeywordType:         "ansi:14",
		NameBuiltin:         "ansi:14",
		NameFunction:        "ansi:10",
		NameClass:           "ansi:10 bold",
		NameDecorator:       "ansi:13",
		LiteralString:       "ansi:10",
		LiteralStringEscape: "ansi:9",
		LiteralNumber:       "ansi:11",
		Comment:             "ansi:8 italic",
		CommentPreproc:      "ansi:13",
		Operator:            "default",
		Punctuation:         "default",
		Text:                "default",
	}
}

func withTerminalProfile(base, detected Theme) Theme {
	base.Terminal = detected.Terminal
	return base
}

func blendTerminalColors(from, to TerminalColor, percent int) TerminalColor {
	fromRGB, _ := rgbForTerminalColor(from)
	toRGB, _ := rgbForTerminalColor(to)
	percent = clampPercent(percent)
	return ColorRGB(
		blendChannel(fromRGB[0], toRGB[0], percent),
		blendChannel(fromRGB[1], toRGB[1], percent),
		blendChannel(fromRGB[2], toRGB[2], percent),
	)
}

const maxThemeFileSize = 1 << 20

// ThemeSource is an immutable, fully validated custom theme revision. Runtime
// profile changes resolve this value without touching the filesystem again.
type ThemeSource struct {
	Name   string
	Path   string
	Digest [sha256.Size]byte
	File   ThemeFile
}

// ThemeResolution is the pure result of resolving a preference against one
// terminal snapshot and, for custom preferences, one accepted source file.
type ThemeResolution struct {
	Theme Theme
	Name  string
}

// ThemePreference separates the persisted setting from the process-only
// ZUT_THEME override. Only dark/light force a running session.
type ThemePreference struct {
	Persisted string
	Effective string
	Forced    bool
}

func ResolveThemePreference(persisted, env string) ThemePreference {
	preference := ThemePreference{Persisted: strings.TrimSpace(persisted), Effective: strings.TrimSpace(persisted)}
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "dark", "light":
		preference.Effective = strings.ToLower(strings.TrimSpace(env))
		preference.Forced = true
	case "", "auto":
		// Persisted setting remains effective.
	}
	if preference.Effective == "" {
		preference.Effective = "auto"
	}
	return preference
}

// LoadThemeSource resolves and validates a custom source. Built-in selections
// have no source and return nil, nil. The file limit prevents a polling reload
// from allocating an unbounded partial write.
func LoadThemeSource(zutHome, preference string) (*ThemeSource, error) {
	path, err := resolveThemePath(zutHome, preference)
	if err != nil || path == "" || strings.HasPrefix(path, "builtin:") {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("theme %s: %w", path, err)
	}
	if info.Size() > maxThemeFileSize {
		return nil, fmt.Errorf("theme %s exceeds %d bytes", path, maxThemeFileSize)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read theme %s: %w", path, err)
	}
	if len(b) > maxThemeFileSize {
		return nil, fmt.Errorf("theme %s exceeds %d bytes", path, maxThemeFileSize)
	}
	var file ThemeFile
	if err := json.Unmarshal(b, &file); err != nil {
		return nil, fmt.Errorf("parse theme %s: %w", path, err)
	}
	if file.Name == "" {
		file.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return &ThemeSource{Name: file.Name, Path: path, Digest: sha256.Sum256(b), File: file}, nil
}

// ResolveTheme performs no I/O. Custom themes always overlay TerminalTheme,
// so omitted roles continue to follow the controlling terminal.
func ResolveTheme(preference string, source *ThemeSource, profile TerminalProfile) ThemeResolution {
	preference = strings.ToLower(strings.TrimSpace(preference))
	switch preference {
	case "", "auto", "default", "system":
		return ThemeResolution{Theme: TerminalTheme(profile), Name: "auto"}
	case "dark":
		return ThemeResolution{Theme: withTerminalProfile(Dark, Theme{Terminal: profile}), Name: "dark"}
	case "light":
		return ThemeResolution{Theme: withTerminalProfile(Light, Theme{Terminal: profile}), Name: "light"}
	}
	if source == nil {
		return ThemeResolution{Theme: TerminalTheme(profile), Name: "auto"}
	}
	base := TerminalTheme(profile)
	base = applyThemeOverrides(base, source.File.Overrides)
	base = applyThemeOverrides(base, source.File.Colors.Base)
	if profileIsLight(profile) {
		if source.File.Colors.HasLight {
			base = applyThemeOverrides(base, source.File.Colors.Light)
		} else if source.File.Colors.HasDark {
			base = applyThemeOverrides(base, source.File.Colors.Dark)
		}
	} else if source.File.Colors.HasDark {
		base = applyThemeOverrides(base, source.File.Colors.Dark)
	} else if source.File.Colors.HasLight {
		base = applyThemeOverrides(base, source.File.Colors.Light)
	}
	return ThemeResolution{Theme: base, Name: source.Name}
}

func profileIsLight(profile TerminalProfile) bool {
	if profile.SchemeKnown {
		return profile.Light
	}
	if profile.HasBackground {
		r, g, b := profile.Background.R, profile.Background.G, profile.Background.B
		return 0.2126*float64(r)+0.7152*float64(g)+0.0722*float64(b) >= 127.5
	}
	return false
}

// DetectThemeWithCustom performs the initial bounded profile query then uses
// the same loader and pure resolver as runtime selection.
func DetectThemeWithCustom(zutHome, preferred string, timeout time.Duration) (Theme, string, error) {
	detected := DetectThemeFromBackground(timeout)
	return LoadThemeFromHome(zutHome, preferred, detected)
}

// LoadThemeFromHome is retained as the startup convenience wrapper. New
// runtime code should keep the ThemeSource and call ResolveTheme directly.
func LoadThemeFromHome(zutHome, preferred string, detected Theme) (Theme, string, error) {
	profile := detected.Terminal
	source, err := LoadThemeSource(zutHome, preferred)
	if err != nil {
		return TerminalTheme(profile), "", err
	}
	returned := ResolveTheme(preferred, source, profile)
	return returned.Theme, returned.Name, nil
}

// AvailableThemes returns built-in and user-installed themes suitable
// for a settings picker. Invalid JSON files are skipped.
func AvailableThemes(zutHome string) []ThemeOption {
	out := []ThemeOption{
		{Value: "auto", Label: "auto", Description: "follow terminal colors and appearance", Builtin: true},
		{Value: "dark", Label: "dark", Description: "built-in dark theme", Builtin: true},
		{Value: "light", Label: "light", Description: "built-in light theme", Builtin: true},
	}
	seen := map[string]bool{"auto": true, "dark": true, "light": true}
	paths, _ := themeFilesIn(filepath.Join(zutHome, "themes"))
	sort.Strings(paths)
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var tf ThemeFile
		if err := json.Unmarshal(b, &tf); err != nil {
			continue
		}
		value := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if value == "" || seen[value] {
			continue
		}
		desc := tf.Description
		if desc == "" {
			desc = path
		}
		out = append(out, ThemeOption{Value: value, Label: value, Description: desc, Path: path})
		seen[value] = true
	}
	return out
}

// ThemeOptionFromFile parses one theme JSON file for picker display.
// value is what will be stored in config; pass an absolute path for
// extension-owned themes so they can be loaded without copying into
// $ZUT_HOME/themes.
func ThemeOptionFromFile(path, value, source string) (ThemeOption, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ThemeOption{}, false
	}
	var tf ThemeFile
	if err := json.Unmarshal(b, &tf); err != nil {
		return ThemeOption{}, false
	}
	if value == "" {
		value = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	label := value
	if tf.Name != "" {
		label = tf.Name
	}
	desc := tf.Description
	if source != "" {
		if desc != "" {
			desc = "from " + source + " — " + desc
		} else {
			desc = "from " + source
		}
	}
	if desc == "" {
		desc = path
	}
	return ThemeOption{Value: value, Label: label, Description: desc, Path: path}, true
}

func ThemeExists(zutHome, preferred string) bool {
	path, err := resolveThemePath(zutHome, preferred)
	return err == nil && path != ""
}

func resolveThemePath(zutHome, preferred string) (string, error) {
	preferred = strings.TrimSpace(preferred)
	switch strings.ToLower(preferred) {
	case "", "auto", "default", "system":
		return "", nil
	case "dark":
		return "builtin:dark", nil
	case "light":
		return "builtin:light", nil
	}
	if preferred != "" && !strings.HasPrefix(preferred, "builtin:") {
		candidates := []string{preferred}
		if filepath.Ext(preferred) == "" {
			candidates = append(candidates,
				filepath.Join(zutHome, "themes", preferred+".json"),
			)
		} else if !filepath.IsAbs(preferred) {
			candidates = append(candidates,
				filepath.Join(zutHome, "themes", preferred),
			)
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c, nil
			}
		}
		return "", fmt.Errorf("theme %q not found", preferred)
	}

	return "", nil
}

func themeFilesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || strings.ToLower(filepath.Ext(e.Name())) != ".json" {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}

func applyThemeOverrides(th Theme, o ThemeOverrides) Theme {
	if o.FG != nil {
		th.FG = o.FG.TerminalColor
	}
	if o.Muted != nil {
		th.Muted = o.Muted.TerminalColor
	}
	if o.Accent != nil {
		th.Accent = o.Accent.TerminalColor
	}
	if o.Background != nil {
		bg := o.Background.TerminalColor
		th.Background = &bg
	}
	if o.User != nil {
		th.User = o.User.TerminalColor
	}
	if o.UserBubbleBG != nil {
		th.UserBubbleBG = o.UserBubbleBG.TerminalColor
	}
	if o.UserBubbleFG != nil {
		th.UserBubbleFG = o.UserBubbleFG.TerminalColor
	}
	if o.Assistant != nil {
		th.Assistant = o.Assistant.TerminalColor
	}
	if o.Tool != nil {
		th.Tool = o.Tool.TerminalColor
	}
	if o.ToolOut != nil {
		th.ToolOut = o.ToolOut.TerminalColor
	}
	if o.Error != nil {
		th.Error = o.Error.TerminalColor
	}
	if o.Warning != nil {
		th.Warning = o.Warning.TerminalColor
	}
	if o.Spinner != nil {
		th.Spinner = o.Spinner.TerminalColor
	}
	if o.ThinkingMax != nil {
		th.ThinkingMax = o.ThinkingMax.TerminalColor
	} else if o.ThinkingMaxCamel != nil {
		th.ThinkingMax = o.ThinkingMaxCamel.TerminalColor
	}
	if o.SelectionBG != nil {
		th.SelectionBG = o.SelectionBG.TerminalColor
	}
	if o.SelectionFG != nil {
		th.SelectionFG = o.SelectionFG.TerminalColor
	}
	if len(o.SpinnerFrames) > 0 {
		th.SpinnerFrames = append([]string(nil), o.SpinnerFrames...)
	}
	if o.SpinnerIntervalMS != nil && *o.SpinnerIntervalMS > 0 {
		th.SpinnerIntervalMS = *o.SpinnerIntervalMS
	}
	if o.SyntaxBaseStyle != nil {
		th.SyntaxBaseStyle = *o.SyntaxBaseStyle
	}
	th.Syntax = applySyntaxOverrides(th.Syntax, o.Syntax)
	return th
}

func applySyntaxOverrides(s SyntaxTheme, o SyntaxThemeOverrides) SyntaxTheme {
	if o.Keyword != nil {
		s.Keyword = *o.Keyword
	}
	if o.KeywordConstant != nil {
		s.KeywordConstant = *o.KeywordConstant
	}
	if o.KeywordDeclaration != nil {
		s.KeywordDeclaration = *o.KeywordDeclaration
	}
	if o.KeywordNamespace != nil {
		s.KeywordNamespace = *o.KeywordNamespace
	}
	if o.KeywordReserved != nil {
		s.KeywordReserved = *o.KeywordReserved
	}
	if o.KeywordType != nil {
		s.KeywordType = *o.KeywordType
	}
	if o.NameBuiltin != nil {
		s.NameBuiltin = *o.NameBuiltin
	}
	if o.NameFunction != nil {
		s.NameFunction = *o.NameFunction
	}
	if o.NameClass != nil {
		s.NameClass = *o.NameClass
	}
	if o.NameDecorator != nil {
		s.NameDecorator = *o.NameDecorator
	}
	if o.LiteralString != nil {
		s.LiteralString = *o.LiteralString
	}
	if o.LiteralStringEscape != nil {
		s.LiteralStringEscape = *o.LiteralStringEscape
	}
	if o.LiteralNumber != nil {
		s.LiteralNumber = *o.LiteralNumber
	}
	if o.Comment != nil {
		s.Comment = *o.Comment
	}
	if o.CommentPreproc != nil {
		s.CommentPreproc = *o.CommentPreproc
	}
	if o.Operator != nil {
		s.Operator = *o.Operator
	}
	if o.Punctuation != nil {
		s.Punctuation = *o.Punctuation
	}
	if o.Text != nil {
		s.Text = *o.Text
	}
	return s
}

func validateExplicitTerminalColor(color TerminalColor) error {
	switch color.Mode {
	case terminalColor256:
		if color.Index < 0 || color.Index > 255 {
			return fmt.Errorf("xterm-256 index %d is outside 0..255", color.Index)
		}
	case terminalColorANSI:
		if _, ok := ansiSGRToXtermIndex(color.Index); !ok {
			return fmt.Errorf("ANSI SGR %d is not a palette color", color.Index)
		}
	case terminalColorRGB:
		if color.R < 0 || color.R > 255 || color.G < 0 || color.G > 255 || color.B < 0 || color.B > 255 {
			return fmt.Errorf("RGB color (%d,%d,%d) is outside 0..255", color.R, color.G, color.B)
		}
	default:
		return errors.New("theme colors must be explicit xterm-256, ANSI, or RGB values")
	}
	return nil
}

func validateThemeFile(file ThemeFile) error {
	for _, overrides := range []ThemeOverrides{file.Overrides, file.Colors.Base, file.Colors.Dark, file.Colors.Light} {
		if err := validateThemeOverrides(overrides); err != nil {
			return err
		}
	}
	return nil
}

func validateThemeOverrides(o ThemeOverrides) error {
	for _, color := range []*TerminalColorValue{o.FG, o.Muted, o.Accent, o.Background, o.User, o.UserBubbleBG, o.UserBubbleFG, o.Assistant, o.Tool, o.ToolOut, o.Error, o.Warning, o.Spinner, o.ThinkingMax, o.ThinkingMaxCamel, o.SelectionBG, o.SelectionFG} {
		if color != nil {
			if err := validateExplicitTerminalColor(color.TerminalColor); err != nil {
				return err
			}
		}
	}
	if len(o.SpinnerFrames) > 64 {
		return errors.New("spinner_frames has more than 64 entries")
	}
	for _, frame := range o.SpinnerFrames {
		if frame == "" || len([]rune(frame)) > 16 {
			return fmt.Errorf("invalid spinner frame %q", frame)
		}
	}
	if o.SpinnerIntervalMS != nil && (*o.SpinnerIntervalMS < 10 || *o.SpinnerIntervalMS > 10000) {
		return fmt.Errorf("spinner_interval_ms %d is outside 10..10000", *o.SpinnerIntervalMS)
	}
	if o.SyntaxBaseStyle != nil && *o.SyntaxBaseStyle != "" {
		if _, ok := styles.Registry[*o.SyntaxBaseStyle]; !ok {
			return fmt.Errorf("unknown syntax_base_style %q", *o.SyntaxBaseStyle)
		}
	}
	for _, entry := range []*string{o.Syntax.Keyword, o.Syntax.KeywordConstant, o.Syntax.KeywordDeclaration, o.Syntax.KeywordNamespace, o.Syntax.KeywordReserved, o.Syntax.KeywordType, o.Syntax.NameBuiltin, o.Syntax.NameFunction, o.Syntax.NameClass, o.Syntax.NameDecorator, o.Syntax.LiteralString, o.Syntax.LiteralStringEscape, o.Syntax.LiteralNumber, o.Syntax.Comment, o.Syntax.CommentPreproc, o.Syntax.Operator, o.Syntax.Punctuation, o.Syntax.Text} {
		if entry == nil {
			continue
		}
		if _, err := chroma.ParseStyleEntry(*entry); err != nil {
			return fmt.Errorf("invalid syntax style %q: %w", *entry, err)
		}
	}
	return nil
}

func IsLightTheme(th Theme) bool {
	return th.FG == Light.FG && th.SelectionBG == Light.SelectionBG && th.SelectionFG == Light.SelectionFG
}

func isLightTheme(th Theme) bool { return IsLightTheme(th) }

func parseHexColor(s string) (TerminalColor, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return TerminalColor{}, false
	}
	var rgb [3]int
	for i := 0; i < 3; i++ {
		v, ok := parseHexByte(s[i*2 : i*2+2])
		if !ok {
			return TerminalColor{}, false
		}
		rgb[i] = v
	}
	return ColorRGB(rgb[0], rgb[1], rgb[2]), true
}

func parseHexByte(s string) (int, bool) {
	var n int
	for _, r := range s {
		n *= 16
		switch {
		case r >= '0' && r <= '9':
			n += int(r - '0')
		case r >= 'a' && r <= 'f':
			n += int(r-'a') + 10
		case r >= 'A' && r <= 'F':
			n += int(r-'A') + 10
		default:
			return 0, false
		}
	}
	return n, true
}
