package usage

import (
	"time"

	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/usagelog"
)

// SrcImport labels a session attributed through its import record: the
// account chosen at `bffs import` / `bffs copy` time (A-15). It ranks below
// SrcLastSession and above SrcLaunchLog.
const SrcImport = "import"

// Attributor answers which account a session belongs to, for the session
// catalog (`bffs sessions`, the MCP tools, the TUI). It uses the evidence
// Collect uses plus import records, first tier that decides wins:
//
//  1. the root's owner — the transcript lives in a full-isolation account's
//     own config tree (SrcRoot);
//  2. the lastSessionId each oauth account's own .claude.json records
//     (SrcLastSession); more than one claimant is corrupt → undecided;
//  3. the import record the session arrived with, attributed to the account
//     chosen at import time, "home" when it went to the unmanaged ~/.claude
//     (SrcImport); a record whose status is skipped names nothing on disk
//     and is ignored;
//  4. launch-log correlation by (cwd, time), exactly as `bffs usage` does
//     it (SrcLaunchLog).
//
// What no tier decides is ("", "") — never a guess. Everything is loaded
// once in NewAttributor; Attribute never touches the disk, so attributing
// hundreds of sessions costs nothing beyond the initial reads. *Attributor
// implements transcripts.Attributor.
type Attributor struct {
	lastIDs map[string][]string           // sid → oauth accounts whose .claude.json names it
	events  []usagelog.Event              // the shim launch log, chronological
	imports map[string]imports.SessionRef // sid → its most recent import record
}

var _ transcripts.Attributor = (*Attributor)(nil)

// NewAttributor loads the per-account lastSessionId sets, the launch log
// and the import records under cfgDir. A broken launch log degrades
// attribution rather than failing, as in Collect; an unreadable imports
// directory is an error.
func NewAttributor(cfgDir string, accs store.Accounts) (*Attributor, error) {
	recs, err := imports.Load(cfgDir)
	if err != nil {
		return nil, err
	}
	events, _ := usagelog.Read(cfgDir)
	return &Attributor{
		lastIDs: lastSessionOwners(cfgDir, accs),
		events:  events,
		imports: imports.BySession(recs),
	}, nil
}

// Attribute names the account of session sid: owner is the root's Owner
// ("" for the shared pool), cwd its effective working directory and
// firstTS its first record's timestamp (both may be empty — the launch-log
// tier then cannot fire). src is the Src* label of the tier that answered.
func (a *Attributor) Attribute(sid, cwd string, firstTS time.Time, owner string) (account, src string) {
	if owner != "" {
		return owner, SrcRoot
	}
	if claimants := a.lastIDs[sid]; len(claimants) == 1 {
		return claimants[0], SrcLastSession
	} else if len(claimants) > 1 {
		return "", ""
	}
	if ref, ok := a.imports[sid]; ok && ref.Record != nil && ref.Session != nil && ref.Session.Status != imports.StatusSkipped {
		if ref.Record.Account == "" {
			return transcripts.HomeName, SrcImport
		}
		return ref.Record.Account, SrcImport
	}
	return attributeByLaunch(cwd, firstTS, a.events)
}

// Claimants lists the oauth accounts whose own .claude.json records sid as
// a project's lastSessionId — Claude's own pointer, written at exit. One
// name is the usual case; several means the files disagree.
func (a *Attributor) Claimants(sid string) []string {
	return append([]string(nil), a.lastIDs[sid]...)
}
