package tui

import (
	"image/color"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

// pair is one semantic colour in its dark-background and
// light-background variants (hex).
type pair struct{ dark, light string }

// palette is a theme: semantic colours, never used alone — every
// coloured signal keeps its glyph or word (tui-design: meaning and
// access). mono keeps the bold/faint/reverse look with no colour.
type palette struct {
	name        string
	accent      pair // focus, keys, prompts, the perspective marker
	accent2     pair // section headers, marks, pins
	ok          pair // live, accepted
	warn        pair // missing here, declined-ish states
	bad         pair // errors, declined
	info        pair // imported
	muted       pair // secondary text, counters, unfocused titles
	border      pair // unfocused frame
	selectionBg pair // the cursor row
	mono        bool
}

// palettes are the built-in themes, in the order T cycles them.
var palettes = []palette{
	{name: "default",
		accent: pair{"#5FAFFF", "#005FAF"}, accent2: pair{"#FFAF5F", "#AF5F00"}, ok: pair{"#5FD75F", "#008700"},
		warn: pair{"#FFD75F", "#AF8700"}, bad: pair{"#FF5F5F", "#D70000"}, info: pair{"#87AFFF", "#0057D9"},
		muted: pair{"#8A8FA8", "#7C7F93"}, border: pair{"#45475A", "#C6C8D1"}, selectionBg: pair{"#2A3B55", "#D6E4FF"}},
	{name: "catppuccin",
		accent: pair{"#89B4FA", "#1E66F5"}, accent2: pair{"#FAB387", "#FE640B"}, ok: pair{"#A6E3A1", "#40A02B"},
		warn: pair{"#F9E2AF", "#DF8E1D"}, bad: pair{"#F38BA8", "#D20F39"}, info: pair{"#74C7EC", "#209FB5"},
		muted: pair{"#9399B2", "#8C8FA1"}, border: pair{"#45475A", "#BCC0CC"}, selectionBg: pair{"#313244", "#CCD0DA"}},
	{name: "gruvbox",
		accent: pair{"#83A598", "#076678"}, accent2: pair{"#FE8019", "#AF3A03"}, ok: pair{"#B8BB26", "#79740E"},
		warn: pair{"#FABD2F", "#B57614"}, bad: pair{"#FB4934", "#9D0006"}, info: pair{"#8EC07C", "#427B58"},
		muted: pair{"#A89984", "#7C6F64"}, border: pair{"#504945", "#D5C4A1"}, selectionBg: pair{"#3C3836", "#EBDBB2"}},
	{name: "tokyo-night",
		accent: pair{"#7AA2F7", "#2E7DE9"}, accent2: pair{"#FF9E64", "#B15C00"}, ok: pair{"#9ECE6A", "#587539"},
		warn: pair{"#E0AF68", "#8C6C3E"}, bad: pair{"#F7768E", "#F52A65"}, info: pair{"#7DCFFF", "#007197"},
		muted: pair{"#7982A9", "#848CB5"}, border: pair{"#3B4261", "#C4C8DA"}, selectionBg: pair{"#283457", "#D5D8E6"}},
	{name: "solarized",
		accent: pair{"#268BD2", "#268BD2"}, accent2: pair{"#CB4B16", "#CB4B16"}, ok: pair{"#859900", "#859900"},
		warn: pair{"#B58900", "#B58900"}, bad: pair{"#DC322F", "#DC322F"}, info: pair{"#2AA198", "#2AA198"},
		muted: pair{"#657B83", "#93A1A1"}, border: pair{"#073642", "#EEE8D5"}, selectionBg: pair{"#073642", "#EEE8D5"}},
	{name: "mono", mono: true},
}

// themeNames lists the built-in themes in cycle order.
func themeNames() []string {
	names := make([]string, 0, len(palettes))
	for _, p := range palettes {
		names = append(names, p.name)
	}
	return names
}

// paletteByName finds a theme; ok is false for a name nobody knows.
func paletteByName(name string) (palette, bool) {
	for _, p := range palettes {
		if p.name == name {
			return p, true
		}
	}
	return palette{}, false
}

// nextThemeName is the theme after cur in cycle order (wrapping).
func nextThemeName(cur string) string {
	for i, p := range palettes {
		if p.name == cur {
			return palettes[(i+1)%len(palettes)].name
		}
	}
	return palettes[0].name
}

// resolveThemeName picks the theme: NO_COLOR forces mono (colour is
// never the only signal, so nothing is lost); else $BFFS_THEME; else the
// theme state.toml names; else the default. An unknown name falls back
// to the default and is reported in note.
func resolveThemeName(stateTheme string, env func(string) string) (name, note string) {
	if env("NO_COLOR") != "" {
		return "mono", ""
	}
	for _, cand := range []struct{ name, src string }{{env("BFFS_THEME"), "BFFS_THEME"}, {stateTheme, "state.toml"}} {
		if cand.name == "" {
			continue
		}
		if _, ok := paletteByName(cand.name); ok {
			return cand.name, ""
		}
		return palettes[0].name, "unknown theme " + strings.TrimSpace(cand.name) + " in " + cand.src + "; known: " + strings.Join(themeNames(), ", ")
	}
	return palettes[0].name, ""
}

// applyTheme sets the package styles from p for a dark or light
// background. Rows are laid out as plain text and styled whole or by
// segment (renderStyled), so the styles never nest.
func applyTheme(p palette, isDark bool) {
	if p.mono {
		styleHeader = lipgloss.NewStyle().Bold(true)
		styleFaint = lipgloss.NewStyle().Faint(true)
		styleCursor = lipgloss.NewStyle().Reverse(true)
		styleMuted = lipgloss.NewStyle().Faint(true).Reverse(true)
		styleSection = lipgloss.NewStyle().Faint(true)
		styleError = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
		styleStatus = lipgloss.NewStyle()
		styleBorder = lipgloss.NewStyle()
		styleBorderFocus = lipgloss.NewStyle()
		styleTitle = lipgloss.NewStyle().Faint(true)
		styleTitleFocus = lipgloss.NewStyle().Bold(true)
		styleCounter = lipgloss.NewStyle().Faint(true)
		styleKey = lipgloss.NewStyle().Bold(true)
		styleDesc = lipgloss.NewStyle().Faint(true)
		styleLive, styleImported, styleMissing, styleMark, stylePin = lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle().Bold(true), lipgloss.NewStyle()
		styleOK, styleBad, styleWarn, styleAccent, styleInfo = lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle().Bold(true), lipgloss.NewStyle()
		stylePrompt = lipgloss.NewStyle().Bold(true)
		return
	}
	ld := lipgloss.LightDark(isDark)
	c := func(pr pair) color.Color { return ld(lipgloss.Color(pr.light), lipgloss.Color(pr.dark)) }
	fg := func(pr pair) lipgloss.Style { return lipgloss.NewStyle().Foreground(c(pr)) }
	styleHeader = fg(p.accent).Bold(true)
	styleFaint = fg(p.muted)
	styleCursor = lipgloss.NewStyle().Background(c(p.selectionBg)).Bold(true)
	styleMuted = fg(p.muted).Background(c(p.border))
	styleSection = fg(p.accent2).Bold(true)
	styleError = fg(p.bad).Bold(true)
	styleStatus = lipgloss.NewStyle()
	styleBorder = fg(p.border)
	styleBorderFocus = fg(p.accent)
	styleTitle = fg(p.muted)
	styleTitleFocus = fg(p.accent).Bold(true)
	styleCounter = fg(p.muted)
	styleKey = fg(p.accent).Bold(true)
	styleDesc = fg(p.muted)
	styleLive = fg(p.ok)
	styleImported = fg(p.info)
	styleMissing = fg(p.warn)
	styleMark = fg(p.accent2).Bold(true)
	stylePin = fg(p.accent2)
	styleOK = fg(p.ok)
	styleBad = fg(p.bad)
	styleWarn = fg(p.warn)
	styleAccent = fg(p.accent)
	styleInfo = fg(p.info)
	stylePrompt = fg(p.accent)
}

// osEnv is the environment lookup resolveThemeName uses at runtime.
func osEnv(k string) string { return os.Getenv(k) }
