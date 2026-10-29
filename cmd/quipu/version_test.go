package main

import "testing"

func TestVersionAndPinning(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		module   string
		settings map[string]string
		version  string
		pin      string
	}{
		{"v0.2.0", nil, "v0.2.0", "v0.2.0"},
		{"v0.1.1-0.20261029120000-0123456789ab", nil, "v0.1.1-0.20261029120000-0123456789ab", "v0.1.1-0.20261029120000-0123456789ab"},
		{"v0.1.1-0.20261029120000-0123456789ab+dirty", nil, "v0.1.1-0.20261029120000-0123456789ab+dirty", ""},
		{"(devel)", map[string]string{"vcs.revision": sha}, sha, sha},
		{"(devel)", map[string]string{"vcs.revision": sha, "vcs.modified": "true"}, sha + "+dirty", ""},
		{"", nil, "0.0.0-dev", ""},
	}
	for _, tc := range cases {
		got := versionFrom(tc.module, tc.settings)
		if got != tc.version || pinnable(got) != tc.pin {
			t.Errorf("versionFrom(%q) = %q pin %q; want %q pin %q", tc.module, got, pinnable(got), tc.version, tc.pin)
		}
	}
}
