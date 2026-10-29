package main

import (
	"runtime/debug"
	"strings"
)

// version is the module version of `go install ...@v0.2.0`, or the version
// Go stamps on a source build: a pseudo-version naming its commit.
var version = buildVersion()

func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "0.0.0-dev"
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	return versionFrom(info.Main.Version, settings)
}

func versionFrom(module string, settings map[string]string) string {
	if module != "" && module != "(devel)" {
		return module
	}
	if revision := settings["vcs.revision"]; revision != "" {
		if settings["vcs.modified"] == "true" {
			return revision + "+dirty"
		}
		return revision
	}
	return "0.0.0-dev"
}

// pinnable returns v when generated workflows can install exactly it, and
// "" for a development build with no tag or clean commit behind it.
func pinnable(v string) string {
	if v == "0.0.0-dev" || strings.Contains(v, "dirty") {
		return ""
	}
	if strings.HasPrefix(v, "v") {
		return v
	}
	if len(v) == 40 && strings.Trim(v, "0123456789abcdef") == "" {
		return v
	}
	return ""
}
