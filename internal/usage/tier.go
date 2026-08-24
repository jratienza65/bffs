package usage

import "encoding/json"

// Tier is the plan/rate-limit tier claude caches for an oauth account,
// parsed from the oauthAccount blob bffs already stores in accounts.toml
// (store.Account.OAuthAccountMeta) — zero new file reads.
type Tier struct {
	User, Org  string
	ExtraUsage bool
}

// ParseTier is tolerant: empty or invalid meta yields the zero Tier.
func ParseTier(oauthAccountMeta string) Tier {
	if oauthAccountMeta == "" {
		return Tier{}
	}
	var m struct {
		User  string `json:"userRateLimitTier"`
		Org   string `json:"organizationRateLimitTier"`
		Extra bool   `json:"hasExtraUsageEnabled"`
	}
	if json.Unmarshal([]byte(oauthAccountMeta), &m) != nil {
		return Tier{}
	}
	return Tier{User: m.User, Org: m.Org, ExtraUsage: m.Extra}
}

// Display renders the tier for tables: user tier, else org tier, else "-",
// with "+extra" when pay-per-use extra usage is enabled.
func (t Tier) Display() string {
	s := t.User
	if s == "" {
		s = t.Org
	}
	if s == "" {
		return "-"
	}
	if t.ExtraUsage {
		s += "+extra"
	}
	return s
}
