package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/jratienza65/bffs/internal/store"
)

// The theme resolves NO_COLOR → mono, then $BFFS_THEME, then state.toml,
// then the default; an unknown name falls back with a note.
func TestResolveThemeName(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if name, note := resolveThemeName("gruvbox", env(map[string]string{"NO_COLOR": "1"})); name != "mono" || note != "" {
		t.Errorf("NO_COLOR: %q %q", name, note)
	}
	if name, note := resolveThemeName("gruvbox", env(map[string]string{"BFFS_THEME": "solarized"})); name != "solarized" || note != "" {
		t.Errorf("env: %q %q", name, note)
	}
	if name, note := resolveThemeName("gruvbox", env(nil)); name != "gruvbox" || note != "" {
		t.Errorf("state: %q %q", name, note)
	}
	if name, note := resolveThemeName("", env(nil)); name != "default" || note != "" {
		t.Errorf("default: %q %q", name, note)
	}
	if name, note := resolveThemeName("neon", env(nil)); name != "default" || !strings.Contains(note, "unknown theme neon in state.toml") || !strings.Contains(note, "mono") {
		t.Errorf("unknown: %q %q", name, note)
	}
	if next := nextThemeName("mono"); next != "default" {
		t.Errorf("cycle should wrap: %q", next)
	}
	if next := nextThemeName("nope"); next != "default" {
		t.Errorf("unknown current should restart the cycle: %q", next)
	}
	if len(themeNames()) != len(palettes) || themeNames()[0] != "default" {
		t.Errorf("names = %v", themeNames())
	}
}

// Every palette applies without a nil colour and colours the semantic
// tokens; mono leaves them uncoloured; light and dark differ.
func TestApplyThemePalettes(t *testing.T) {
	t.Cleanup(func() { applyTheme(palettes[0], true) })
	for _, p := range palettes {
		applyTheme(p, true)
		dark := styleAccent.Render("x")
		applyTheme(p, false)
		light := styleAccent.Render("x")
		if p.mono {
			if strings.Contains(dark, "38;2;") || strings.Contains(light, "38;2;") {
				t.Errorf("%s: mono must not colour: %q", p.name, dark)
			}
			continue
		}
		if !strings.Contains(dark, "38;2;") && !strings.Contains(dark, "38;5;") {
			t.Errorf("%s: accent not coloured: %q", p.name, dark)
		}
		if p.accent.dark != p.accent.light && dark == light {
			t.Errorf("%s: light and dark variants render the same", p.name)
		}
		if lipgloss.Width(styleOK.Render("✓")) != 1 || lipgloss.Width(styleBad.Render("✗")) != 1 {
			t.Errorf("%s: coloured glyphs changed width", p.name)
		}
	}
}

// T cycles the theme at once and saves it to state.toml; the header
// names a non-default theme.
func TestThemeKey(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	t.Setenv("BFFS_THEME", "")
	t.Setenv("NO_COLOR", "")
	h := f.start("sessions")
	t.Cleanup(func() { applyTheme(palettes[0], true) })
	if h.a.svc.theme != "default" {
		t.Fatalf("theme = %q", h.a.svc.theme)
	}
	raw := h.a.View().Content
	if !strings.Contains(raw, "38;2;") {
		t.Fatal("the default theme should colour the frame")
	}
	h.keys("T")
	if h.a.svc.theme != "catppuccin" {
		t.Fatalf("T should cycle to catppuccin, got %q", h.a.svc.theme)
	}
	wantAll(t, h.view(), "theme catppuccin (saved to state.toml", "theme catppuccin")
	st, err := store.LoadState(f.cfgDir)
	if err != nil || st.Theme != "catppuccin" {
		t.Fatalf("state.toml theme = %q err=%v", st.Theme, err)
	}
	// Cycle to mono: no colour at all, and the terminal's answer about
	// its background does not bring it back.
	for h.a.svc.theme != "mono" {
		h.keys("T")
	}
	h.send(tea.BackgroundColorMsg{Color: lipgloss.Color("#ffffff")})
	if raw := h.a.View().Content; strings.Contains(raw, "38;2;") {
		t.Error("mono must not colour")
	}
	if h.a.svc.isDark {
		t.Error("a white background should read as light")
	}
}
