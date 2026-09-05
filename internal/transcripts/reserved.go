package transcripts

import "strings"

// ReservedProjectEntries names the entries Claude (or bffs) places inside
// projects/ that are not session slugs — they must never be listed, exported
// or used as a slug. Keys are lower-case; look them up through IsReserved.
// internal/bundle keeps an equal private copy (it may not import this
// package), pinned by a test on both sides.
var ReservedProjectEntries = map[string]bool{
	"memory":              true,
	"tiny_memory":         true,
	"bagel":               true,
	"cloud-snapshots":     true,
	"bridge-pointer.json": true,
	".session-aliases":    true,
}

const (
	setAsideReplaced = ".bffs-replaced-"
	setAsideTmp      = ".bffs-tmp"
)

// IsSetAside reports whether name is a bffs-owned set-aside or staging
// entry: "<x>.bffs-replaced-<epochms>" (a file or directory an import
// displaced), "<x>.bffs-tmp" or "<x>.bffs-tmp-<rand>" (a transactional
// commit in flight). Neither bffs nor Claude ever deletes them on its own.
func IsSetAside(name string) bool {
	n := strings.ToLower(name)
	if i := strings.LastIndex(n, setAsideReplaced); i >= 0 {
		rest := n[i+len(setAsideReplaced):]
		return rest != "" && allDigits(rest)
	}
	if strings.HasSuffix(n, setAsideTmp) {
		return true
	}
	if i := strings.LastIndex(n, setAsideTmp+"-"); i >= 0 && i+len(setAsideTmp)+1 < len(n) {
		return true
	}
	return false
}

// IsReserved reports whether a projects/ (or slug-dir) entry is not a
// session or slug: one of ReservedProjectEntries (case-insensitively) or a
// set-aside name. Listing, finding and exporting skip these; the bundle
// layout check whitelists them.
func IsReserved(name string) bool {
	return ReservedProjectEntries[strings.ToLower(name)] || IsSetAside(name)
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
