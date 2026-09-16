package tui

import (
	"fmt"
	"strings"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// rootRow is one projects/ pool.
type rootRow struct {
	root  transcripts.Root
	label string
}

func (r *rootRow) FilterValue() string { return r.label }
func (r *rootRow) render(width int) string {
	return truncate(r.label, width)
}

// rootLabel names a root for the roots screen: the shared pool with the
// accounts attached to it, a full-isolation account's own tree, an
// orphan session dir, or the unmanaged home dir. Names are sanitised: an
// orphan's is a directory name read from disk.
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

// shortRootLabel is the compact form the other screens' headers use.
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
