package main

// Where the config lives and which copy may be trusted. Most commands read the
// worktree's copy, but a setting that relaxes enforcement must come from the
// default branch: a worktree copy could otherwise turn it on.

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/isnakolah/quipu/internal/model"
	"gopkg.in/yaml.v3"
)

// rootConfigPath is the config's home. configPath is the older one, which
// repositories adopted before quipu.yml keep using.
const rootConfigPath = "quipu.yml"

// configSourceWorktree marks a config read from the checkout itself.
const configSourceWorktree = "worktree"

func pickConfigFile(root, legacy bool) (string, error) {
	switch {
	case root && legacy:
		return "", fmt.Errorf("both %s and %s exist; keep only %s", rootConfigPath, configPath, rootConfigPath)
	case root:
		return rootConfigPath, nil
	case legacy:
		return configPath, nil
	}
	return "", fmt.Errorf("no %s or %s: %w", rootConfigPath, configPath, os.ErrNotExist)
}

// configFile names the config in the checkout at top.
func configFile(top string) (string, error) {
	_, rootErr := os.Stat(filepath.Join(top, rootConfigPath))
	_, legacyErr := os.Stat(filepath.Join(top, configPath))
	return pickConfigFile(rootErr == nil, legacyErr == nil)
}

// hasConfig reports whether Quipu governs the checkout at top. Two configs
// count: that is an error to report, not a repository to leave unguarded.
func hasConfig(top string) bool {
	_, err := configFile(top)
	return !errors.Is(err, os.ErrNotExist)
}

// decodeConfig rejects unknown keys, so a misspelled flag cannot silently stay off.
func decodeConfig(raw []byte) (model.Config, error) {
	var c model.Config
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return c, err
	}
	return c, c.Validate()
}

// configAtRef reads the config committed at ref.
func (w worktree) configAtRef(ref string) (model.Config, string, error) {
	root, rootErr := w.git("show", ref+":"+rootConfigPath)
	legacy, legacyErr := w.git("show", ref+":"+configPath)
	name, err := pickConfigFile(rootErr == nil, legacyErr == nil)
	if err != nil {
		return model.Config{}, "", err
	}
	raw := root
	if name == configPath {
		raw = legacy
	}
	c, err := decodeConfig([]byte(raw))
	if err != nil {
		return c, "", fmt.Errorf("%s:%s: %w", ref, name, err)
	}
	return c, ref + ":" + name, nil
}

// localDefaultBranch names origin's default branch from git's own record,
// never from a config a worktree can edit. Agents cannot change this ref.
func (w worktree) localDefaultBranch() string {
	ref, err := w.git("symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(ref, "origin/")
}

// defaultBranch also asks origin, for clones that never recorded its HEAD.
func (w worktree) defaultBranch() string {
	if branch := w.localDefaultBranch(); branch != "" {
		return branch
	}
	output, err := w.git("ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(output, "\n") {
		if ref, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			if branch, _, ok := strings.Cut(ref, "\t"); ok {
				return branch
			}
		}
	}
	return ""
}

// publishedConfig is the config on the default branch as this clone last saw
// it, without touching the network, so the guard can call it on every write.
func (w worktree) publishedConfig() (model.Config, bool) {
	branch := w.localDefaultBranch()
	if branch == "" {
		return model.Config{}, false
	}
	c, _, err := w.configAtRef("origin/" + branch)
	return c, err == nil
}

// loadEnforcedConfig returns the config to enforce and where it came from.
// With an origin, that is the default branch's copy, fetched first when
// refresh is set. With an origin but no readable copy, it is the worktree's
// with every relaxing setting off. Only a repository with no origin at all
// has nothing to compare against, and uses the worktree's as it stands.
func loadEnforcedConfig(w worktree, refresh bool) (model.Config, string, error) {
	local, localErr := loadConfigAt(w.Top)
	if _, err := w.git("remote", "get-url", "origin"); err != nil {
		return local, configSourceWorktree, localErr
	}
	if branch := w.defaultBranch(); branch != "" {
		if refresh {
			_, _ = w.git("fetch", "--quiet", "origin", branch)
		}
		c, source, err := w.configAtRef("origin/" + branch)
		if err == nil {
			return c, source, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return c, "", err
		}
	}
	return local.WithoutRelaxations(), configSourceWorktree + ", relaxing settings off", localErr
}

// configSetting is one effective value and where it came from.
type configSetting struct {
	Key    string `json:"key"`
	Value  any    `json:"value"`
	Source string `json:"source"`
}

// configSourceDefault marks a value no config file sets.
const configSourceDefault = "default"

// optionDefaults are the values Quipu uses for settings left unset.
var optionDefaults = map[string]any{
	"ready_by":                         "owner",
	"merge":                            "owner",
	"required_checks":                  []string{"test"},
	"policy_exempt_authors":            []string{},
	"commit_format":                    "none",
	"issues_by":                        "owner",
	"triage_by":                        "owner",
	"update_behind":                    "off",
	"merge_wait":                       "foreground",
	"merge_without_ci":                 false,
	"local_check":                      "none",
	"merge_bypass_rulesets":            false,
	"guard_reads":                      "closed",
	"workflows.runs_on":                map[string][]string{"default": {"ubuntu-latest"}},
	"workflows.quipu_version":          "main",
	"workflows.schedule":               model.DefaultSchedule,
	"workflows.writer_on_pull_request": true,
	"workflows.defects_from":           []string{"CI"},
	"workflows.runner_start":           "none",
}

// configValues flattens a config to its set keys, with workflows one level deep.
func configValues(c model.Config) (map[string]any, error) {
	raw, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	values := map[string]any{}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	if workflows, ok := values["workflows"].(map[string]any); ok {
		delete(values, "workflows")
		for key, value := range workflows {
			values["workflows."+key] = value
		}
	}
	return values, nil
}

func configSettings(c model.Config, source string) ([]configSetting, error) {
	values, err := configValues(c)
	if err != nil {
		return nil, err
	}
	var settings []configSetting
	for key, value := range values {
		settings = append(settings, configSetting{key, value, source})
	}
	for key, value := range optionDefaults {
		if _, set := values[key]; !set {
			settings = append(settings, configSetting{key, value, configSourceDefault})
		}
	}
	sort.Slice(settings, func(i, j int) bool { return settings[i].Key < settings[j].Key })
	return settings, nil
}

// configDifferences lists the keys where the worktree's copy disagrees with
// the config being enforced. They have no effect until merged.
func configDifferences(enforced, local model.Config) ([]string, error) {
	want, err := configValues(enforced)
	if err != nil {
		return nil, err
	}
	got, err := configValues(local)
	if err != nil {
		return nil, err
	}
	differs := []string{}
	for key, value := range got {
		if !reflect.DeepEqual(value, want[key]) {
			differs = append(differs, key)
		}
	}
	for key := range want {
		if _, set := got[key]; !set {
			differs = append(differs, key)
		}
	}
	sort.Strings(differs)
	return differs, nil
}

// configCommand is `quipu config`: each effective value and where it came from.
func configCommand(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	w, err := currentWorktree()
	if err != nil {
		return err
	}
	c, source, err := loadEnforcedConfig(w, true)
	if err != nil {
		return err
	}
	settings, err := configSettings(c, source)
	if err != nil {
		return err
	}
	report := map[string]any{"repository": c.Owner + "/" + c.Repository, "source": source, "settings": settings}
	if source != configSourceWorktree {
		if local, err := loadConfigAt(w.Top); err == nil {
			differs, err := configDifferences(c, local)
			if err != nil {
				return err
			}
			report["worktree_differs"] = differs
		} else {
			report["worktree_error"] = err.Error()
		}
	}
	return printValue(report, *asJSON)
}
