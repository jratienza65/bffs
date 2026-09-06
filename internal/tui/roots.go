package tui

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// accountRow is one perspective of panel 1: a bffs account (the root it
// browses and the .claude.json its per-account facts come from), "home"
// for an unmanaged ~/.claude no account is attached to, or an orphan
// session dir (read-only). Selecting a row picks the pool the other
// panels list and the column the drift tables mark.
type accountRow struct {
	name   string
	kind   string // "partial", "full", "api key", "home", "orphan"
	root   transcripts.Root
	active bool
	note   string // why it cannot be browsed ("" = fine)
}

func (r *accountRow) FilterValue() string { return r.name + " " + r.kind }

func (r *accountRow) render(width int) string {
	name, right := r.parts(width)
	if right == "" {
		return name
	}
	return name + " " + right
}

// renderStyled colours the active marker and mutes the kind.
func (r *accountRow) renderStyled(width int) string {
	name, right := r.parts(width)
	if right == "" {
		return name
	}
	if r.active {
		return name + " " + styleOK.Render(right)
	}
	return name + " " + styleFaint.Render(right)
}

func (r *accountRow) parts(width int) (name, right string) {
	right = r.kind
	if r.active {
		right = "● active"
	}
	nameW := width - 1 - lipgloss.Width(right)
	if nameW < 6 {
		return truncate(transcripts.Sanitize(r.name), width), ""
	}
	return pad(transcripts.Sanitize(r.name), nameW), right
}

// accountRows lists every account of accounts.toml (the active one
// first), then "home" when the unmanaged ~/.claude has no account
// attached to it, then orphan session dirs.
func accountRows(svc *services) []*accountRow {
	names := append([]string{}, svc.accs.Names()...)
	sort.Strings(names)
	var rows []*accountRow
	for _, name := range names {
		acc, _ := svc.accs.Get(name)
		r := &accountRow{name: name, active: name == svc.state.Active}
		switch acc.Type {
		case store.TypeOAuth:
			r.kind = string(store.ResolveIsolation(acc.Isolation, svc.state.Isolation))
		default:
			r.kind = "api key"
		}
		root, err := rootForAccount(svc, name)
		if err != nil {
			r.note = err.Error()
		}
		r.root = root
		rows = append(rows, r)
	}
	for _, root := range svc.roots {
		switch {
		case root.Orphan:
			rows = append(rows, &accountRow{name: root.Owner, kind: "orphan", root: root, note: "orphan session dir: read-only"})
		case root.Owner == "" && !root.Shared:
			rows = append(rows, &accountRow{name: transcripts.HomeName, kind: "home", root: root})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].active && !rows[j].active })
	return rows
}

// rootLabel names a root: the shared pool with the accounts attached to
// it, a full-isolation account's own tree, an orphan session dir, or the
// unmanaged home dir. Names are sanitised: an orphan's is a directory
// name read from disk.
func rootLabel(r transcripts.Root) string {
	switch {
	case r.Orphan:
		return fmt.Sprintf("orphan: %s (read-only)", transcripts.Sanitize(r.Owner))
	case r.Owner != "":
		return transcripts.Sanitize(r.Owner) + " [full isolation]"
	case r.Shared && len(r.Accounts) > 0:
		return fmt.Sprintf("shared pool (%s) — accounts: %s", shortPath(r.ConfigDir), accountList(r.Accounts))
	default:
		return fmt.Sprintf("home (%s)", shortPath(r.ConfigDir))
	}
}

// shortRootLabel is the compact form the tables use.
func shortRootLabel(r transcripts.Root) string {
	switch {
	case r.Orphan:
		return "orphan: " + transcripts.Sanitize(r.Owner) + ", read-only"
	case r.Owner != "":
		return "account: " + transcripts.Sanitize(r.Owner)
	case r.Shared && len(r.Accounts) > 0:
		return "shared pool: " + accountList(r.Accounts)
	default:
		return "home: " + shortPath(r.ConfigDir)
	}
}

// accountList joins account names for display.
func accountList(names []string) string {
	return transcripts.Sanitize(strings.Join(names, ", "))
}
