package tui

import (
	"image/color"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// pair is one semantic colour in its dark-background and
// light-background variants (hex).
type pair struct{ dark, light string }

// rung is what a role falls back to on a terminal with sixteen
// colours: an index into the base palette, per background.
//
// Without this, Lip Gloss picks the nearest of the sixteen to the hex,
// which is where a theme quietly loses: the default palette's warn, bad
// and section headers all land on bright red, and on its light variant
// the pane borders land on bright white — invisible on a white
// terminal (internal/tui/testdata/sixteen-colour-rungs.txt). The rungs
// are a property of the role rather than of the theme, because with
// sixteen colours there is nothing left to theme: the sixteen are the
// terminal's own, and a user who themed them has already chosen.
type rung struct{ dark, light int }

var rungs = struct {
	accent, accent2, ok, warn, bad, info, muted, border, selectionBg rung
}{
	accent:  rung{12, 4}, // bright blue on dark, blue on light
	accent2: rung{11, 5}, // orange has no rung: bright yellow, magenta
	ok:      rung{10, 2},
	warn:    rung{3, 3},
	bad:     rung{9, 1},
	info:    rung{14, 6},
	muted:   rung{7, 8}, // a grey on each: light on dark, dark on light
	border:  rung{8, 7}, // dimmer than the text it frames, on both
	// The cursor row paints a background and lets the terminal's own
	// foreground ride on it, so the rung has to contrast with that
	// foreground rather than with the hex: blue under white text on a
	// dark terminal, bright cyan under black text on a light one.
	selectionBg: rung{4, 14},
}

// themeProfile is the colour profile the styles are built for. A
// non-terminal stdout — a test, a pipe, a golden — reads as no colour
// at all, which would leave every token unpainted, so it is clamped up
// to 256: what the frame is *drawn* with stays the full-fidelity
// colour, and what the terminal can show is Bubble Tea's business at
// write time.
var themeProfile = colorprofile.ANSI256

// detectProfile reads the profile of the terminal bffs is drawing to.
// Only "not a terminal at all" is clamped up — a terminal that really
// has sixteen colours must be told so, or the rungs never apply.
func detectProfile() colorprofile.Profile {
	switch p := colorprofile.Detect(os.Stdout, os.Environ()); p {
	case colorprofile.NoTTY, colorprofile.Ascii:
		return colorprofile.ANSI256
	default:
		return p
	}
}

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
		styleOK, styleBad, styleWarn, styleAccent = lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle().Bold(true)
		stylePrompt = lipgloss.NewStyle().Bold(true)
		styleStrong, styleEmph = lipgloss.NewStyle().Bold(true), lipgloss.NewStyle().Italic(true)
		styleCodeSpan, styleLink = lipgloss.NewStyle(), lipgloss.NewStyle().Underline(true)
		styleSelection = lipgloss.NewStyle().Reverse(true)
		return
	}
	ld := lipgloss.LightDark(isDark)
	complete := lipgloss.Complete(themeProfile)
	c := func(pr pair, r rung) color.Color {
		idx := r.light
		if isDark {
			idx = r.dark
		}
		hex := ld(lipgloss.Color(pr.light), lipgloss.Color(pr.dark))
		return complete(lipgloss.ANSIColor(idx), hex, hex)
	}
	fg := func(pr pair, r rung) lipgloss.Style { return lipgloss.NewStyle().Foreground(c(pr, r)) }
	styleHeader = fg(p.accent, rungs.accent).Bold(true)
	styleFaint = fg(p.muted, rungs.muted)
	styleCursor = lipgloss.NewStyle().Background(c(p.selectionBg, rungs.selectionBg)).Bold(true)
	styleMuted = fg(p.muted, rungs.muted).Background(c(p.border, rungs.border))
	styleSection = fg(p.accent2, rungs.accent2).Bold(true)
	styleError = fg(p.bad, rungs.bad).Bold(true)
	styleStatus = lipgloss.NewStyle()
	styleBorder = fg(p.border, rungs.border)
	styleBorderFocus = fg(p.accent, rungs.accent)
	styleTitle = fg(p.muted, rungs.muted)
	styleTitleFocus = fg(p.accent, rungs.accent).Bold(true)
	styleCounter = fg(p.muted, rungs.muted)
	styleKey = fg(p.accent, rungs.accent).Bold(true)
	styleDesc = fg(p.muted, rungs.muted)
	styleLive = fg(p.ok, rungs.ok)
	styleImported = fg(p.info, rungs.info)
	styleMissing = fg(p.warn, rungs.warn)
	styleMark = fg(p.accent2, rungs.accent2).Bold(true)
	stylePin = fg(p.accent2, rungs.accent2)
	styleOK = fg(p.ok, rungs.ok)
	styleBad = fg(p.bad, rungs.bad)
	styleWarn = fg(p.warn, rungs.warn)
	styleAccent = fg(p.accent, rungs.accent)
	stylePrompt = fg(p.accent, rungs.accent)
	// Inline markup carries two signals like everything else: strong is
	// bold on the body colour rather than another accent, so a heading
	// still outranks it, and a link is the one underlined thing.
	styleStrong = lipgloss.NewStyle().Bold(true)
	styleEmph = lipgloss.NewStyle().Italic(true)
	styleCodeSpan = fg(p.accent2, rungs.accent2)
	styleLink = fg(p.info, rungs.info).Underline(true)
	// Reverse video rather than a background token: a selection has to
	// read as selected on every palette and on a terminal with sixteen
	// colours, and swapping the cell's own colours always does.
	styleSelection = lipgloss.NewStyle().Reverse(true)
}

// osEnv is the environment lookup resolveThemeName uses at runtime.
func osEnv(k string) string { return os.Getenv(k) }
