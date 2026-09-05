package porter

import (
	"github.com/jratienza65/bffs/internal/resolver"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// ResumeAccount names the account a `claude --resume` must run as to see a
// session in root from cwd — the BFFS_ACCOUNT prefix of the verify line
// (rehome.VerifyCommand). A root with an owner (full isolation, or an
// orphan) is that owner; the home or shared root is whatever
// resolver.Resolve picks for cwd, and "" when it picks nothing or fails
// (a plain `claude` then runs with the same credentials). An empty cwd
// resolves against the process's working directory, the place a pending
// session would be resumed from.
func ResumeAccount(cfgDir string, root transcripts.Root, cwd string) string {
	if root.Owner != "" {
		return root.Owner
	}
	if cwd == "" {
		cwd = "."
	}
	res, err := resolver.Resolve(cfgDir, cwd)
	if err != nil {
		return ""
	}
	return res.Account.Name
}
