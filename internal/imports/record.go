// Package imports is bffs's memory of what it imported or copied: one JSON
// record per bundle under <cfgDir>/imports/<bundleID>.json, mode 0600.
//
// A record answers, for every session and memory directory that arrived
// through `bffs import` or `bffs copy`, where it came from (machine, user,
// home, account), which account and root it was placed under, the mapping
// applied, and its status (placed / rehomed / pending / skipped). Consumers
// are `bffs sessions imports`, `sessions list --pending-rehome`, `bffs rehome
// --bundle/--suggest`, the usage attributor (tier 3) and the skill.
//
// The package is a leaf: it depends on fsutil only, so transcripts, usage,
// rehome and porter can all read records without an import cycle.
package imports

import "time"

// Subdir is the directory under the bffs config dir that holds records.
const Subdir = "imports"

// Record kinds.
const (
	KindImport = "import" // arrived through `bffs import` (file, stdin or LAN)
	KindCopy   = "copy"   // same-machine `bffs copy` between roots
)

// Session / memory statuses.
const (
	StatusPlaced  = "placed"  // landed under the directory it names here (identity placement)
	StatusRehomed = "rehomed" // moved to a mapped directory with a relocated stamp
	StatusPending = "pending" // landed as-is; its cwd does not exist here yet
	StatusSkipped = "skipped" // not written (collision policy, live, refusal)
)

// Record is one import or local copy. JSON field names are snake_case and
// stable: other tools may read the files.
type Record struct {
	BundleID   string    `json:"bundle_id"`
	Kind       string    `json:"kind"` // KindImport | KindCopy
	ImportedAt time.Time `json:"imported_at"`
	Account    string    `json:"account"`   // account chosen at import time ("" = unmanaged home)
	DestRoot   string    `json:"dest_root"` // projects/ directory the entries landed in
	Source     Source    `json:"source"`
	Sessions   []Session `json:"sessions"`
	Memories   []Memory  `json:"memories,omitempty"`
	Mapping    []Mapping `json:"mapping,omitempty"`
}

// Source describes where a bundle was produced. Paths are informational
// (compared and printed), never joined into local paths.
type Source struct {
	Hostname      string `json:"hostname"`
	User          string `json:"user"`
	Home          string `json:"home"`
	OS            string `json:"os"`
	Account       string `json:"account"`
	BFFSVersion   string `json:"bffs_version"`
	ClaudeVersion string `json:"claude_version"`
}

// Session is one imported conversation and where it ended up.
type Session struct {
	ID        string    `json:"id"`
	OldCwd    string    `json:"old_cwd"`
	OldSlug   string    `json:"old_slug"`
	NewCwd    string    `json:"new_cwd"`
	Slug      string    `json:"slug"`
	Title     string    `json:"title,omitempty"`
	GitRemote string    `json:"git_remote,omitempty"`
	Status    string    `json:"status"` // StatusPlaced | StatusRehomed | StatusPending | StatusSkipped
	OrigMtime time.Time `json:"orig_mtime"`
}

// Memory is one imported auto-memory directory.
type Memory struct {
	OldCwd string `json:"old_cwd"`
	Dir    string `json:"dir"` // local memory directory it was merged into or staged at
	Status string `json:"status"`
}

// Mapping is one prefix rule (Old → New) applied during placement; the
// longest matching Old prefix wins.
type Mapping struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// SessionRef points at a session inside the record it belongs to.
type SessionRef struct {
	Record  *Record
	Session *Session
}
