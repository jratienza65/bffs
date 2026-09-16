package tui

import (
	"fmt"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
)

// panelID numbers the side panels the way the keys do.
type panelID int

const (
	panelAccounts panelID = iota
	panelProjects
	panelItems // sessions | memory, tabbed
	panelCount
)

// panel is one framed list of the side column. It owns a list.Model
// (rows, cursor, filter, paging) and knows how to draw itself expanded
// (the focused panel: the list, its title row carrying a one-line
// status or the filter input) or collapsed (the rows from the selected
// one down, the selection muted, no cursor).
type panel struct {
	id      panelID
	name    string // the frame title: "accounts", "projects", "sessions"…
	list    list.Model
	status  string // the list's title row: "16 sessions · 2 live"
	empty   string // shown when there is nothing to list
	loading bool
	width   int // inner width

	// The view is scrolled independently of the selection (the wheel
	// moves it, the cursor only drags it along when it would leave the
	// screen): offset is the first row drawn, rowsH the rows the last
	// render had room for, lastIndex the selection it was drawn with.
	offset    int
	rowsH     int
	lastIndex int
}

func newPanel(id panelID, name, singular, plural, empty string) *panel {
	p := &panel{id: id, name: name, empty: empty, list: newList(nil, singular, plural)}
	p.list.SetShowStatusBar(false)
	p.list.SetShowPagination(false)
	p.list.Styles.Title = styleTitle
	return p
}

// setRows replaces the panel's rows, keeping the cursor inside the list.
func (p *panel) setRows(rows []row) tea.Cmd {
	items := make([]list.Item, 0, len(rows))
	for _, r := range rows {
		items = append(items, r)
	}
	cmd := p.list.SetItems(items)
	if p.list.Index() >= len(items) {
		p.list.Select(max(0, len(items)-1))
	}
	return cmd
}

// rows returns every row in list order (filtered or not).
func (p *panel) rows() []row {
	items := p.list.Items()
	out := make([]row, 0, len(items))
	for _, it := range items {
		if r, ok := it.(row); ok {
			out = append(out, r)
		}
	}
	return out
}

// selected is the row under the cursor, nil when the list is empty.
func (p *panel) selected() row {
	r, _ := p.list.SelectedItem().(row)
	return r
}

// count is "cursor/visible" for the frame title.
func (p *panel) count() string {
	n := len(p.list.VisibleItems())
	if n == 0 {
		return "0"
	}
	return fmt.Sprintf("%d/%d", p.list.Index()+1, n)
}

// setSize gives the list its inner width and the rows it may draw.
func (p *panel) setSize(width, height int) {
	p.width = width
	// A conservative row count for the loaders that run before the
	// next render (the status row may take one); body refines it.
	p.rowsH = max(1, height-1)
	p.list.SetSize(width, max(1, height))
	p.list.Title = truncate(p.status, max(0, width-2))
}

// setStatus updates the title row.
func (p *panel) setStatus(s string) {
	p.status = s
	p.list.Title = truncate(s, max(0, p.width-2))
}

// filtering reports whether the panel's filter input has the keyboard.
func (p *panel) filtering() bool { return p.list.SettingFilter() }

// label is the frame title's left part: the number and the name (or
// the tabs).
func (p *panel) label(tabs string) string {
	if tabs != "" {
		return fmt.Sprintf("%d %s", int(p.id)+1, tabs)
	}
	return fmt.Sprintf("%d %s", int(p.id)+1, p.name)
}

// counter is the frame title's right part: cursor/visible, or … while
// the rows load.
func (p *panel) counter() string {
	if p.loading {
		return "…"
	}
	return p.count()
}

// titleRow is the panel's status line: the filter input while one is
// being typed, the filter's result while one is applied, else the
// one-line status the workspace set.
func (p *panel) titleRow(width int) string {
	if p.list.SettingFilter() {
		return truncate(p.list.FilterInput.View(), width)
	}
	if p.list.FilterState() == list.FilterApplied {
		return styleTitle.Render(truncate(fmt.Sprintf("“%s” · %d of %d", p.list.FilterValue(), len(p.list.VisibleItems()), len(p.list.Items())), width))
	}
	return styleTitle.Render(truncate(p.status, width))
}

// clampOffset keeps the view inside the rows.
func (p *panel) clampOffset(rows, height int) {
	if p.offset > rows-height {
		p.offset = rows - height
	}
	if p.offset < 0 {
		p.offset = 0
	}
}

// follow scrolls just enough for the selection to be on screen; the
// keyboard drags the view this way, the wheel never does.
func (p *panel) follow(height int) {
	i := p.list.Index()
	switch {
	case i < p.offset:
		p.offset = i
	case i >= p.offset+height:
		p.offset = i - height + 1
	}
}

// scroll moves the view by n rows, leaving the selection where it is.
func (p *panel) scroll(n int) {
	p.offset += n
	p.clampOffset(len(p.list.VisibleItems()), max(1, p.rowsH))
}

// window is the rows on screen plus a screen of look-ahead either way —
// what the lazy title loader resolves.
func (p *panel) window() (start, end int) {
	n := len(p.list.VisibleItems())
	h := max(1, p.rowsH)
	return max(0, p.offset-h), min(n, p.offset+2*h)
}

// body draws height lines of width cells: the focused panel leads with
// its status row and shows the cursor, an unfocused one mutes it.
func (p *panel) body(width, height int, focused bool) []string {
	if height <= 0 {
		return nil
	}
	lines := make([]string, 0, height)
	rows := height
	if focused {
		rows--
		lines = append(lines, p.titleRow(width))
	}
	p.rowsH = max(1, rows)
	items := p.list.VisibleItems()
	if len(items) == 0 {
		text := p.empty
		if p.loading {
			text = "loading…"
		}
		return fill(append(lines, styleFaint.Render(truncate(text, width))), height)
	}
	// The selection drags the view along when it moves off screen, and
	// an unfocused panel always shows it — it is the context for the
	// panels below.
	if idx := p.list.Index(); idx != p.lastIndex || !focused {
		p.lastIndex = idx
		p.follow(p.rowsH)
	}
	p.clampOffset(len(items), p.rowsH)
	for i := p.offset; i < len(items) && len(lines) < height; i++ {
		r, ok := items[i].(row)
		if !ok {
			continue
		}
		switch {
		case i == p.list.Index() && focused:
			lines = append(lines, styleCursor.Render("> "+pad(r.render(width-2), width-2)))
		case i == p.list.Index():
			lines = append(lines, styleMuted.Render(pad("  "+r.render(width-2), width)))
		case !focused:
			lines = append(lines, styleFaint.Render(pad("  "+r.render(width-2), width)))
		default:
			if sr, ok := items[i].(styledRow); ok {
				lines = append(lines, "  "+cell(sr.renderStyled(width-2), width-2))
			} else {
				lines = append(lines, "  "+pad(r.render(width-2), width-2))
			}
		}
	}
	return fill(lines, height)
}

// fill pads or cuts lines to exactly n entries.
func fill(lines []string, n int) []string {
	if len(lines) > n {
		return lines[:n]
	}
	for len(lines) < n {
		lines = append(lines, "")
	}
	return lines
}
