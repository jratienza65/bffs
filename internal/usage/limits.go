package usage

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// Limit-message matchers are data, not architecture: Claude Code's wording
// will drift, and adding a marker should be a one-line change. Verified
// real formats (digits redacted):
//
//	You've hit your session limit · resets 3:00am (Asia/Manila)
//	You've hit your weekly limit · resets Jun 12 at 3pm (Asia/Manila)
var limitTextPatterns = []struct {
	re   *regexp.Regexp
	kind string
}{
	{regexp.MustCompile(`(?i)you'?ve hit your session limit`), "session"},
	{regexp.MustCompile(`(?i)you'?ve hit your weekly limit`), "weekly"},
}

var resetRe = regexp.MustCompile(`(?i)resets\s+(.+?)\s*\(([^)]+)\)`)

// parseLimitEvent interprets a rate_limit API-error record's content. The
// record itself already proves a limit was hit, so unparseable content still
// yields an event — just with Kind "unknown" and no reset time.
func parseLimitEvent(ts time.Time, content json.RawMessage) LimitEvent {
	ev := LimitEvent{TS: ts, Kind: "unknown"}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ev
	}
	for _, b := range blocks {
		if b.Text == "" {
			continue
		}
		for _, p := range limitTextPatterns {
			if p.re.MatchString(b.Text) {
				ev.Kind = p.kind
			}
		}
		if ev.ResetAt.IsZero() {
			if m := resetRe.FindStringSubmatch(b.Text); m != nil {
				ev.ResetAt = parseReset(ts, m[1], m[2])
			}
		}
	}
	return ev
}

// parseReset turns "3:00am" / "Jun 12 at 3pm" plus an IANA zone name into an
// absolute time, anchored to the record's date. Zero on any failure — the
// event's Kind survives without a reset time.
func parseReset(ts time.Time, when, tzName string) time.Time {
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return time.Time{}
	}
	recLocal := ts.In(loc)

	// Time-only ("3:00am", "3am"): same day, or tomorrow if already past.
	for _, layout := range []string{"3:04pm", "3pm"} {
		if tm, err := time.ParseInLocation(layout, strings.ToLower(when), loc); err == nil {
			r := time.Date(recLocal.Year(), recLocal.Month(), recLocal.Day(), tm.Hour(), tm.Minute(), 0, 0, loc)
			if !r.After(recLocal) {
				r = r.Add(24 * time.Hour)
			}
			return r
		}
	}
	// Date form ("Jun 12 at 3pm"): record's year, bumped across a year wrap.
	for _, layout := range []string{"Jan 2 at 3pm", "Jan 2 at 3:04pm"} {
		if tm, err := time.ParseInLocation(layout, when, loc); err == nil {
			r := time.Date(recLocal.Year(), tm.Month(), tm.Day(), tm.Hour(), tm.Minute(), 0, 0, loc)
			if r.Before(recLocal.AddDate(0, -1, 0)) {
				r = r.AddDate(1, 0, 0)
			}
			return r
		}
	}
	return time.Time{}
}
