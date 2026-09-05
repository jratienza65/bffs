package bundle

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// Kinds returned by ClassifyName for names inside a bundle.
const (
	NameManifest    = "manifest"     // manifest.json
	NameTranscript  = "transcript"   // projects/<slug>/<sid>.jsonl
	NameSidecar     = "sidecar"      // projects/<slug>/<sid>/...
	NameFileHistory = "file-history" // file-history/<sid>/<hash>@v<n>
	NamePlans       = "plans"        // plans/<planSlug>*.md
	NameHistory     = "history"      // history/<sid>.jsonl
	NameTasks       = "tasks"        // tasks/<sid>/**
	NameMemory      = "memory"       // memory/<slug>/**.md
	NameReserved    = "reserved"     // claudejson/, session-env/, user-memory/
)

// Entry kinds (Entry.Kind).
const (
	EntrySession = "session"
	EntryMemory  = "memory"
)

const (
	maxNameLen      = 1024
	maxComponentLen = 255
	maxSlugLen      = 240
	maxPlanSlugLen  = 80
	maxAgentIDLen   = 64
	manifestName    = "manifest.json"
)

// reservedSlugs is a private copy of transcripts.ReservedProjectEntries —
// bundle may not import transcripts. TestReservedSlugsEqual pins the two
// maps equal. Keys are lower-case; compare after strings.ToLower.
var reservedSlugs = map[string]bool{
	"memory":              true,
	"tiny_memory":         true,
	"bagel":               true,
	"cloud-snapshots":     true,
	"bridge-pointer.json": true,
	".session-aliases":    true,
}

var sidecarDirs = map[string]bool{
	"subagents":     true,
	"workflows":     true,
	"tool-results":  true,
	"remote-agents": true,
	"mcp-tasks":     true,
}

var sidecarFiles = map[string]bool{
	"custom-title.json": true,
	".ccr-tip.json":     true,
	".precompact.json":  true,
	"sent-prefix.json":  true,
}

// ValidateEntryName reports whether name is acceptable as a tar entry name
// in a bundle, independent of the namespace grammar (ClassifyName adds
// that). The rules are lexical and OS-independent so a bundle valid on one
// platform is valid on every other: every byte in [A-Za-z0-9._@/-],
// filepath.IsLocal, path.Clean(name) == name, no leading "/", no empty, "."
// or ".." component, no Windows device name as a component, no component
// ending in "." (Windows strips trailing dots, so "x.md." would land on
// "x.md"), each component ≤ 255 bytes, the whole name ≤ 1024 bytes.
func ValidateEntryName(name string) error {
	if name == "" {
		return fmt.Errorf("empty entry name")
	}
	for i := 0; i < len(name); i++ {
		if !nameByte(name[i]) {
			return fmt.Errorf("entry name %q contains %q; allowed bytes are [A-Za-z0-9._@/-]", name, name[i])
		}
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("entry name %q is %d bytes; max %d", name, len(name), maxNameLen)
	}
	if name[0] == '/' {
		return fmt.Errorf("entry name %q is absolute", name)
	}
	if path.Clean(name) != name {
		return fmt.Errorf("entry name %q is not clean (want %q)", name, path.Clean(name))
	}
	if !filepath.IsLocal(name) {
		return fmt.Errorf("entry name %q is not local", name)
	}
	for _, c := range strings.Split(name, "/") {
		switch c {
		case "":
			return fmt.Errorf("entry name %q has an empty component", name)
		case ".", "..":
			return fmt.Errorf("entry name %q has a %q component", name, c)
		}
		if len(c) > maxComponentLen {
			return fmt.Errorf("entry name %q has a %d-byte component; max %d", name, len(c), maxComponentLen)
		}
		if isWindowsDevice(c) {
			return fmt.Errorf("entry name %q has a windows device name component %q", name, c)
		}
		if c[len(c)-1] == '.' {
			return fmt.Errorf("entry name %q has a component %q ending in a dot, which windows strips", name, c)
		}
	}
	return nil
}

// ClassifyName maps a tar entry name onto the bundle namespace grammar (see
// the package documentation). It returns the kind (one of the Name*
// constants), the session id for sid-keyed kinds, and the project slug for
// slug-keyed kinds ("" where the grammar carries none — plans files carry a
// plan slug that Validate checks against Entry.PlanSlug instead). Names
// outside the grammar, reserved or malformed slugs, lock files under tasks/
// and excluded memory files are errors; ValidateEntryName runs first, so a
// nil error here implies a nil error there.
func ClassifyName(name string) (kind, sid, slug string, err error) {
	if err := ValidateEntryName(name); err != nil {
		return "", "", "", err
	}
	parts := strings.Split(name, "/")
	outside := func() (string, string, string, error) {
		return "", "", "", fmt.Errorf("entry name %q is outside the bundle namespace", name)
	}
	switch parts[0] {
	case manifestName:
		if len(parts) == 1 {
			return NameManifest, "", "", nil
		}
	case "projects":
		if len(parts) < 3 {
			return outside()
		}
		slug := parts[1]
		if err := checkSlug(slug); err != nil {
			return "", "", "", fmt.Errorf("entry name %q: %w", name, err)
		}
		if len(parts) == 3 {
			s, ok := strings.CutSuffix(parts[2], ".jsonl")
			if ok && isUUID(s) {
				return NameTranscript, s, slug, nil
			}
			return outside()
		}
		sid := parts[2]
		if !isUUID(sid) {
			return outside()
		}
		rest := parts[3:]
		if len(rest) == 1 {
			f := rest[0]
			if sidecarFiles[f] || (strings.HasSuffix(f, ".cast") && len(f) > len(".cast")) {
				return NameSidecar, sid, slug, nil
			}
			return outside()
		}
		if sidecarDirs[rest[0]] {
			return NameSidecar, sid, slug, nil
		}
		return outside()
	case "file-history":
		if len(parts) == 3 && isUUID(parts[1]) && isFileHistoryName(parts[2]) {
			return NameFileHistory, parts[1], "", nil
		}
	case "plans":
		if len(parts) == 2 && isPlanFile(parts[1]) {
			return NamePlans, "", "", nil
		}
	case "history":
		if len(parts) == 2 {
			s, ok := strings.CutSuffix(parts[1], ".jsonl")
			if ok && isUUID(s) {
				return NameHistory, s, "", nil
			}
		}
	case "tasks":
		if len(parts) < 3 || !isUUID(parts[1]) {
			return outside()
		}
		for _, c := range parts[2:] {
			if c == ".lock" || strings.HasSuffix(c, ".lock") {
				return "", "", "", fmt.Errorf("entry name %q is a lock file; tasks lock files never travel", name)
			}
		}
		return NameTasks, parts[1], "", nil
	case "memory":
		if len(parts) < 3 {
			return outside()
		}
		slug := parts[1]
		if err := checkSlug(slug); err != nil {
			return "", "", "", fmt.Errorf("entry name %q: %w", name, err)
		}
		rest := parts[2:]
		for i, c := range rest {
			lower := strings.ToLower(c)
			if lower == "proposals" && i < len(rest)-1 {
				return "", "", "", fmt.Errorf("entry name %q is under a proposals/ directory, which is never exported", name)
			}
			if strings.HasPrefix(lower, "index") {
				return "", "", "", fmt.Errorf("entry name %q looks like a memory index cache, which is never exported", name)
			}
		}
		last := rest[len(rest)-1]
		if !strings.HasSuffix(last, ".md") || len(last) <= len(".md") {
			return outside()
		}
		return NameMemory, "", slug, nil
	case "claudejson":
		if len(parts) == 2 {
			s, ok := strings.CutSuffix(parts[1], ".json")
			if ok && checkSlug(s) == nil {
				return NameReserved, "", s, nil
			}
		}
	case "session-env":
		if len(parts) >= 3 && isUUID(parts[1]) {
			return NameReserved, parts[1], "", nil
		}
	case "user-memory":
		if len(parts) >= 2 {
			return NameReserved, "", "", nil
		}
	}
	return outside()
}

func nameByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '.', b == '_', b == '@', b == '/', b == '-':
		return true
	}
	return false
}

// isWindowsDevice mirrors the reserved-name rule filepath.IsLocal applies on
// Windows (CON, PRN, AUX, NUL, COM1-9, LPT1-9, with anything after the
// first dot ignored) so every platform rejects the same names.
func isWindowsDevice(c string) bool {
	base := c
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	switch len(base) {
	case 3:
		switch strings.ToUpper(base) {
		case "CON", "PRN", "AUX", "NUL":
			return true
		}
	case 4:
		up := strings.ToUpper(base)
		if (strings.HasPrefix(up, "COM") || strings.HasPrefix(up, "LPT")) && up[3] >= '1' && up[3] <= '9' {
			return true
		}
	}
	return false
}

// checkSlug enforces ^[A-Za-z0-9_-]{1,240}$ and the reserved list.
func checkSlug(s string) error {
	if s == "" || len(s) > maxSlugLen {
		return fmt.Errorf("slug %q must be 1-%d characters", s, maxSlugLen)
	}
	for i := 0; i < len(s); i++ {
		if !slugByte(s[i]) {
			return fmt.Errorf("slug %q contains %q; allowed are [A-Za-z0-9_-]", s, s[i])
		}
	}
	if reservedSlugs[strings.ToLower(s)] {
		return fmt.Errorf("slug %q is a reserved projects/ entry", s)
	}
	return nil
}

func slugByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_', b == '-':
		return true
	}
	return false
}

// isUUID accepts the canonical lowercase 8-4-4-4-12 form only.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !isLowerHex(s[i]) {
				return false
			}
		}
	}
	return true
}

func isLowerHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isLowerHex(s[i]) {
			return false
		}
	}
	return true
}

// isFileHistoryName matches ^[0-9a-f]{16}@v[0-9]+$.
func isFileHistoryName(s string) bool {
	if len(s) < 16+2+1 {
		return false
	}
	for i := 0; i < 16; i++ {
		if !isLowerHex(s[i]) {
			return false
		}
	}
	if s[16] != '@' || s[17] != 'v' {
		return false
	}
	for i := 18; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isPlanSlug matches ^[a-z0-9-]{1,80}$.
func isPlanSlug(s string) bool {
	if s == "" || len(s) > maxPlanSlugLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if !((b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-') {
			return false
		}
	}
	return true
}

func isAgentID(s string) bool {
	if s == "" || len(s) > maxAgentIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !slugByte(s[i]) {
			return false
		}
	}
	return true
}

// isPlanFile matches <planSlug>.md, <planSlug>-agent-<id>.md and
// <planSlug>.workshop.md.
func isPlanFile(f string) bool {
	stem, ok := strings.CutSuffix(f, ".md")
	if !ok {
		return false
	}
	if ws, ok := strings.CutSuffix(stem, ".workshop"); ok {
		return isPlanSlug(ws)
	}
	if isPlanSlug(stem) {
		return true
	}
	if i := strings.LastIndex(stem, "-agent-"); i > 0 {
		return isPlanSlug(stem[:i]) && isAgentID(stem[i+len("-agent-"):])
	}
	return false
}

// planFileBelongs reports whether the plans/ file name f is one of the
// three shapes for planSlug.
func planFileBelongs(f, planSlug string) bool {
	if !isPlanSlug(planSlug) {
		return false
	}
	switch {
	case f == planSlug+".md", f == planSlug+".workshop.md":
		return true
	case strings.HasPrefix(f, planSlug+"-agent-") && strings.HasSuffix(f, ".md"):
		return isAgentID(strings.TrimSuffix(strings.TrimPrefix(f, planSlug+"-agent-"), ".md"))
	}
	return false
}
