// Package rehome is the write side of bffs's session and memory transfer:
// it lands staged sessions inside a Claude config dir transactionally
// (CommitSession), repairs what an interrupted commit left behind
// (Recover), merges imported auto-memory (MergeMemory), appends prompt
// history (AppendHistory), and renders the verification command every
// import ends with (VerifyCommand). The mtime rules that keep imported
// transcripts inside Claude's retention window live here too
// (MtimePolicy, TranscriptFloor).
//
// This is the M4 half of the package — everything porter.Import needs.
// In-place rehoming of local sessions (PlanRehome/Apply, AppendRelocated,
// memory merge, path rewrites, Suggest) arrives with M6; only the types it
// shares with porter (Mapping, Suggestion, Candidate, MemoryMerge) are
// declared here already.
//
// Rules the package is built on (plan §9): the destination is written only
// through an os.Root over the config dir; the transcript is the last file
// of a session to land, so a session is either whole or absent; nothing the
// user owns is ever deleted — displaced entries are set aside as
// "<name>.bffs-replaced-<epochms>" and in-flight entries carry ".bffs-tmp",
// both invisible to Claude's listing (transcripts.IsSetAside); imported
// memory is prompt content, so its "pinned:" frontmatter is neutralised
// unless the user explicitly trusts it.
//
// Dependency rule: rehome imports transcripts, claudejson, fsutil and
// imports — never porter, cmd, usage or bundle.
package rehome

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// Mapping is one prefix rule (Old → New) applied during placement; the
// parser and applier arrive with M6. It is the imports.Mapping type so an
// import record stores exactly what was applied.
type Mapping = imports.Mapping

// MemoryMode says what MergeMemory does when the destination memory
// directory already exists.
type MemoryMode string

const (
	// MemorySkip writes the imported memory only when the destination
	// directory does not exist yet.
	MemorySkip MemoryMode = "skip"
	// MemoryOverwrite sets an existing destination aside as
	// "memory.bffs-replaced-<epochms>" (never deleted) and writes fresh.
	MemoryOverwrite MemoryMode = "overwrite"
	// MemoryMerge merges file by file. The mode is accepted by the type so
	// callers can carry it, but the merge itself arrives with M6; M4
	// returns an error for it.
	MemoryMerge MemoryMode = "merge"
)

// MemoryMove reports what MergeMemory did. Names are relative to the
// memory directory and slash-separated. Added lists files written under
// their own name (a created MEMORY.md included), Renamed lists files that
// landed as "<name>.imported-<id8>.md", Unchanged lists files that were
// not written. IndexAppended is set when an existing MEMORY.md gained an
// imported section (M6 merge). Remaining is the absolute-path scan of the
// destination after a write, for the user to review; it is nil when
// nothing was written.
type MemoryMove struct {
	From, To      string
	Added         []string
	Renamed       []string
	Unchanged     []string
	IndexAppended bool
	Remaining     []transcripts.PathRef
}

// Suggestion is the set of local directories an imported project might
// live in, offered when a bundle's cwd does not exist here. Suggest itself
// arrives with M6; the types are declared now because porter.Placer takes
// them.
type Suggestion struct {
	OldCwd     string
	Candidates []Candidate
}

// Candidate is one suggested directory and why it was suggested.
type Candidate struct {
	Dir, Reason string
}

// Name pieces bffs stamps on entries it owns. transcripts.IsSetAside
// recognises both families; a test pins that.
const (
	// tmpSuffix marks an entry a commit is still building.
	tmpSuffix = ".bffs-tmp"
	// replacedInfix precedes the epoch-millisecond stamp of a displaced entry.
	replacedInfix = ".bffs-replaced-"
	// importedInfix precedes the bundle id prefix on a file that collided
	// with a different existing one.
	importedInfix = ".imported-"
)

// SetAsideName returns the name a displaced entry is renamed to:
// "<name>.bffs-replaced-<epochms>" with the stamp taken from now. Callers
// that set a colliding transcript, sidecar or memory directory aside use
// it so every set-aside on disk has one shape.
func SetAsideName(name string, now time.Time) string {
	return name + replacedInfix + strconv.FormatInt(now.UnixMilli(), 10)
}

var (
	uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	id8Re  = regexp.MustCompile(`^[0-9a-f]{8}$`)
	slugRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
)

func isUUID(s string) bool { return uuidRe.MatchString(s) }

func validateSessionID(sid string) error {
	if !isUUID(sid) {
		return fmt.Errorf("session id %q is not a lowercase uuid", sid)
	}
	return nil
}

func validateID8(id8 string) error {
	if !id8Re.MatchString(id8) {
		return fmt.Errorf("bundle id prefix %q is not eight lowercase hex digits", id8)
	}
	return nil
}

func validateSlug(slug string) error {
	if !slugRe.MatchString(slug) || transcripts.IsReserved(slug) {
		return fmt.Errorf("slug %q is not a projects/ entry name", slug)
	}
	return nil
}
