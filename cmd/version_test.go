package cmd

import (
	"runtime/debug"
	"testing"
)

// The version a binary reports comes from the link-time stamp when a
// release build set one, else from the build info: the module version a
// `go install <module>@<version>` build records, else the commit. It is
// never a literal carried in the source, which is how a 0.3.0 download
// once reported 0.1.0.
func TestVersionString(t *testing.T) {
	vcs := func(rev string, dirty bool) *debug.BuildInfo {
		bi := &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: rev}}}
		if dirty {
			bi.Settings = append(bi.Settings, debug.BuildSetting{Key: "vcs.modified", Value: "true"})
		}
		return bi
	}
	module := func(v string) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Path: "github.com/jratienza65/bffs", Version: v}}
	}
	cases := []struct {
		name  string
		stamp string
		bi    *debug.BuildInfo
		ok    bool
		want  string
	}{
		{"release stamp wins", "0.3.0", module("v0.2.0"), true, "0.3.0"},
		{"stamp without build info", "0.3.0", nil, false, "0.3.0"},
		{"go install at a tag", "", module("v0.3.0"), true, "0.3.0"},
		{"go install at a pseudo-version", "", module("v0.3.1-0.20260910072000-e0df8b3f9393"), true, "0.3.1-0.20260910072000-e0df8b3f9393"},
		{"checkout build", "", vcs("e0df8b3f93393489dc5dc541ad475e2fb827b043", false), true, "dev+e0df8b3f9339"},
		{"dirty checkout build", "", vcs("e0df8b3f93393489dc5dc541ad475e2fb827b043", true), true, "dev+e0df8b3f9339.dirty"},
		{"devel with no vcs stamps", "", module("(devel)"), true, "dev"},
		{"no build info at all", "", nil, false, "dev"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := versionString(c.stamp, c.bi, c.ok); got != c.want {
				t.Errorf("versionString(%q, …) = %q, want %q", c.stamp, got, c.want)
			}
		})
	}
}

// Whatever the build, the binary reports something and cobra prints it.
func TestVersionIsReported(t *testing.T) {
	if Version == "" || rootCmd.Version != Version {
		t.Errorf("Version = %q, rootCmd.Version = %q", Version, rootCmd.Version)
	}
	// The skill frontmatter takes a release number or nothing at all.
	switch v := releaseVersion(); {
	case v == "":
	case v[0] < '0' || v[0] > '9':
		t.Errorf("releaseVersion() = %q, which is not a version number", v)
	}
}
