package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
)

// panelID numbers the side panels the way the keys do.
type panelID int

const (
	panelRoots panelID = iota
	panelProjects
	panelItems // sessions | memory, tabbed
	panelFiles // artifacts of a session, references of a memory file
	panelCount
)

// panel is one framed list of the side column. It owns a list.Model
// (rows, cursor, filter, paging) and knows how to draw itself expanded
// (the focused panel: the list) or collapsed (the selected row and what
// follows it, without a cursor).
type panel struct {
	id      panelID
	name    string // the frame title: "roots", "projects", "sessions"…
	list    list.Model
	header  string // column header in the list's title row ("" = the name)
	empty   string // shown when there is nothing to list
	loading bool
	width   int // inner width
}

func newPanel(id panelID, name, singular, plural, empty string) *panel {
	p := &panel{id: id, name: name, empty: empty, list: newList(nil, singular, plural)}
	p.list.SetShowStatusBar(false)
	p.list.SetShowPagination(false)
	p.list.Styles.Title = styleFaint
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
	p.list.SetSize(width, max(1, height))
	title := p.header
	if title == "" {
		title = p.name
	}
	p.list.Title = truncate(title, max(0, width-2))
}

// filtering reports whether the panel's filter input has the keyboard.
func (p *panel) filtering() bool { return p.list.SettingFilter() }

// title is the frame title: number, name, tabs and the count.
func (p *panel) title(tabs string) string {
	t := fmt.Sprintf("%d %s", int(p.id)+1, p.name)
	if tabs != "" {
		t = fmt.Sprintf("%d %s", int(p.id)+1, tabs)
	}
	if p.loading {
		return t + " …"
	}
	return t + " " + p.count()
}

// body draws height lines of width cells: the list when focused, else
// the rows from the cursor down, the cursor row marked but not styled.
func (p *panel) body(width, height int, focused bool) []string {
	if height <= 0 {
		return nil
	}
	items := p.list.VisibleItems()
	if len(items) == 0 {
		text := p.empty
		if p.loading {
			text = "loading…"
		}
		return fill([]string{styleFaint.Render(truncate(text, width))}, height)
	}
	if focused {
		return fill(strings.Split(p.list.View(), "\n"), height)
	}
	lines := make([]string, 0, height)
	start := p.list.Index()
	if start < 0 {
		start = 0
	}
	for i := start; i < len(items) && len(lines) < height; i++ {
		r, ok := items[i].(row)
		if !ok {
			continue
		}
		mark := "  "
		if i == p.list.Index() {
			mark = "> "
		}
		lines = append(lines, styleFaint.Render(pad(mark+r.render(width-2), width)))
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
