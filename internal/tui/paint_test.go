package tui

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// The tests here read the escapes a frame is painted with, which the
// goldens in frame_test.go strip. Two things can only be seen this way:
// what a colour becomes on a terminal that has sixteen of them, and
// whether a bar is one painted surface or a row of segments with
// unpainted gaps between them.

// sgrRun is a stretch of cells drawn under one SGR sequence.
type sgrRun struct {
	sgr  string // the sequence in force, "" after a reset
	text string
}

// sgrRuns splits a painted line into its runs. Every styled string
// bffs writes carries its own full sequence and resets afterwards
// (styles never nest — see styles.go), so the last sequence seen is the
// whole state of the cells that follow it.
func sgrRuns(line string) []sgrRun {
	var runs []sgrRun
	var b strings.Builder
	cur := ""
	flush := func() {
		if b.Len() > 0 {
			runs = append(runs, sgrRun{sgr: cur, text: b.String()})
			b.Reset()
		}
	}
	for i := 0; i < len(line); {
		if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '[' {
			j := i + 2
			for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
				j++
			}
			if j < len(line) {
				if seq := line[i : j+1]; line[j] == 'm' {
					flush()
					if seq == "\x1b[m" || seq == "\x1b[0m" {
						cur = ""
					} else {
						cur = seq
					}
				}
				i = j + 1
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		b.WriteRune(r)
		i += size
	}
	flush()
	return runs
}

// painting reports whether cells drawn under sgr get a background of
// their own — a real one, or the terminal's colours swapped. Extended
// colours are skipped over by their argument count so that a component
// of a foreground colour (`38;2;42;…`) is never read as a background.
func painting(sgr string) bool {
	params := strings.Split(strings.TrimSuffix(strings.TrimPrefix(sgr, "\x1b["), "m"), ";")
	for i := 0; i < len(params); i++ {
		switch params[i] {
		case "38", "48", "58":
			bg := params[i] == "48"
			switch {
			case i+1 < len(params) && params[i+1] == "5":
				i += 2
			case i+1 < len(params) && params[i+1] == "2":
				i += 4
			}
			if bg {
				return true
			}
		case "7": // reverse video: the mono theme's cursor row
			return true
		default:
			n, err := strconv.Atoi(params[i])
			if err == nil && ((n >= 40 && n <= 47) || (n >= 100 && n <= 107)) {
				return true
			}
		}
	}
	return false
}

// TestCursorRowIsOneSurface scans every painted line of every golden
// state for a hole: two painted stretches with unpainted cells between
// them that are not a pane border. A cursor row composed of separately
// styled segments looks right in a stripped golden and shows a gap in
// the bar on a terminal — the kind of defect only the escapes carry.
func TestCursorRowIsOneSurface(t *testing.T) {
	for name, tweak := range goldenStates() {
		t.Run(name, func(t *testing.T) {
			for i, line := range strings.Split(painted(t, 120, 32, tweak), "\n") {
				runs := sgrRuns(line)
				last := -1
				for j, r := range runs {
					if !painting(r.sgr) {
						continue
					}
					if last >= 0 {
						gap := ""
						for _, between := range runs[last+1 : j] {
							gap += between.text
						}
						if !strings.ContainsAny(gap, "│┤├") {
							t.Errorf("line %d: hole %q between two painted stretches: %q … %q",
								i, gap, r.text, runs[last].text)
						}
					}
					last = j
				}
			}
		})
	}
}

// TestSixteenColourRungs pins what each theme token becomes on a
// terminal with sixteen colours. A hex pair is chosen for a truecolour
// terminal and downsampled by the nearest-colour rule at write time, so
// a palette can read well in one profile and lose a distinction — or a
// whole bar — in the other. The golden makes every such change visible
// in a diff; `make golden` rewrites it.
func TestSixteenColourRungs(t *testing.T) {
	prev := themeProfile
	t.Cleanup(func() { themeProfile = prev; applyTheme(palettes[0], true) })
	var b strings.Builder
	b.WriteString("token                 truecolour                   256           16\n")
	for _, p := range palettes {
		for _, dark := range []bool{true, false} {
			mode := "dark"
			if !dark {
				mode = "light"
			}
			fmt.Fprintf(&b, "\n[%s %s]\n", p.name, mode)
			// The 16-colour column is what a terminal with sixteen
			// colours is *given* — the role's rung — not what the hex
			// happens to downsample to, which is the whole point of
			// the rungs.
			themeProfile = colorprofile.ANSI256
			applyTheme(p, dark)
			full := map[string]string{}
			at256 := map[string]string{}
			for _, tok := range themeTokens() {
				full[tok.name] = firstSGR(tok.style.Render("x"))
				at256[tok.name] = firstSGR(downsample(tok.style.Render("x"), colorprofile.ANSI256))
			}
			themeProfile = colorprofile.ANSI
			applyTheme(p, dark)
			for _, tok := range themeTokens() {
				fmt.Fprintf(&b, "%-20s  %-26s  %-12s  %s\n", tok.name,
					spellEscapes(full[tok.name]), spellEscapes(at256[tok.name]),
					spellEscapes(firstSGR(downsample(tok.style.Render("x"), colorprofile.ANSI))))
			}
		}
	}
	checkGolden(t, "sixteen-colour-rungs", b.String())
}

// themeTokens names the semantic styles as applyTheme leaves them; it
// reads the package variables, so call it after applyTheme.
func themeTokens() []struct {
	name  string
	style lipgloss.Style
} {
	return []struct {
		name  string
		style lipgloss.Style
	}{
		{"header", styleHeader}, {"faint", styleFaint},
		{"cursor row", styleCursor}, {"muted selection", styleMuted},
		{"section", styleSection}, {"error", styleError},
		{"border", styleBorder}, {"border focus", styleBorderFocus},
		{"title", styleTitle}, {"title focus", styleTitleFocus},
		{"counter", styleCounter}, {"key", styleKey}, {"desc", styleDesc},
		{"live", styleLive}, {"imported", styleImported}, {"missing", styleMissing},
		{"mark", styleMark}, {"pin", stylePin},
		{"ok", styleOK}, {"bad", styleBad}, {"warn", styleWarn},
		{"accent", styleAccent}, {"prompt", stylePrompt},
	}
}

// firstSGR is the sequence a styled string opens with, "-" when the
// style paints nothing.
func firstSGR(s string) string {
	for _, r := range sgrRuns(s) {
		if r.sgr != "" {
			return r.sgr
		}
	}
	return "-"
}

// spellEscapes writes ESC as \e so a golden stays readable.
func spellEscapes(s string) string { return strings.ReplaceAll(s, "\x1b", `\e`) }

// TestFooterKeysStayBoldWithoutColour pins the one distinction that has
// to survive when colour does not: the footer's keys are bold and their
// descriptions are not, so `x more · ? keys` still reads as keys and
// words under NO_COLOR, on a 2-colour terminal, and in a pipe.
func TestFooterKeysStayBoldWithoutColour(t *testing.T) {
	frame := downsample(painted(t, 120, 32, nil), colorprofile.Ascii)
	var footer string
	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(ansi.Strip(line), "? keys") {
			footer = line
		}
	}
	if footer == "" {
		t.Fatal("no footer line in the frame")
	}
	if strings.Contains(footer, "\x1b[38") || strings.Contains(footer, "\x1b[48") {
		t.Errorf("colour survived the Ascii profile: %q", spellEscapes(footer))
	}
	var bold, plain []string
	for _, r := range sgrRuns(footer) {
		switch {
		case strings.Contains(r.sgr, "1"):
			bold = append(bold, strings.TrimSpace(r.text))
		case strings.TrimSpace(r.text) != "":
			plain = append(plain, strings.TrimSpace(r.text))
		}
	}
	if len(bold) == 0 {
		t.Fatalf("no bold run in the footer: %q", spellEscapes(footer))
	}
	for _, key := range []string{"x", "?", "q"} {
		if !slicesContains(bold, key) {
			t.Errorf("key %q is not bold in the footer; bold runs: %q, plain: %q", key, bold, plain)
		}
	}
}

func slicesContains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// The rungs exist to keep roles apart on a terminal with sixteen
// colours, where a nearest-colour match collapses them: the golden
// records what each one became, and this records why they may not be
// equal.
func TestRungsKeepRolesApart(t *testing.T) {
	for _, dark := range []bool{true, false} {
		mode := "dark"
		if !dark {
			mode = "light"
		}
		pick := func(r rung) int {
			if dark {
				return r.dark
			}
			return r.light
		}
		roles := map[string]int{
			"accent": pick(rungs.accent), "accent2": pick(rungs.accent2),
			"ok": pick(rungs.ok), "warn": pick(rungs.warn), "bad": pick(rungs.bad),
			"info": pick(rungs.info),
		}
		seen := map[int]string{}
		for name, idx := range roles {
			if other, ok := seen[idx]; ok {
				t.Errorf("%s: %s and %s share rung %d — the two states are indistinguishable there", mode, other, name, idx)
			}
			seen[idx] = name
		}
		if pick(rungs.border) == pick(rungs.muted) {
			t.Errorf("%s: the frame and the text it frames share rung %d", mode, pick(rungs.border))
		}
		// The cursor row paints a background and lets the terminal's
		// own foreground ride on it, so it may never be the colour that
		// foreground is written on.
		bg := pick(rungs.selectionBg)
		if (dark && bg == 0) || (!dark && (bg == 7 || bg == 15)) {
			t.Errorf("%s: the cursor row's background is rung %d, which is the terminal's own", mode, bg)
		}
	}
}

// BFFS_ASCII is the opt-in for a terminal whose font has no symbols: no
// glyph bffs draws may be outside ASCII when it is set. The separator
// and the state marks all come from one set (glyphs.go), so this is a
// scan of the whole frame rather than a list of call sites.
func TestAsciiModeDrawsOnlyAscii(t *testing.T) {
	t.Setenv("BFFS_ASCII", "1")
	for name, tweak := range goldenStates() {
		t.Run(name, func(t *testing.T) {
			frame := frameAt(t, 120, 32, tweak)
			for i, line := range strings.Split(frame, "\n") {
				for _, r := range line {
					if r > 126 {
						t.Fatalf("line %d draws %q with BFFS_ASCII=1: %q", i, string(r), line)
					}
				}
			}
		})
	}
}
