package model

import (
	"fmt"
	"strings"
	"time"
)

const ConfigVersion = 1

type Config struct {
	SchemaVersion    int      `yaml:"schema_version" json:"schema_version"`
	Owner            string   `yaml:"owner" json:"owner"`
	Repository       string   `yaml:"repository" json:"repository"`
	Project          Project  `yaml:"project" json:"project"`
	Areas            []string `yaml:"areas" json:"areas"`
	WikiMode         string   `yaml:"wiki_mode" json:"wiki_mode"`
	DefaultBranch    string   `yaml:"default_branch" json:"default_branch"`
	WriterVersion    string   `yaml:"writer_version" json:"writer_version"`
	AuthorizedActors []string `yaml:"authorized_actors" json:"authorized_actors"`
	Policy           Policy   `yaml:"policy" json:"policy"`
	// ReceiptsTrustedBefore keeps receipts posted before the Writer moved to
	// its bot identity trusted. Later receipts count only from the bot.
	ReceiptsTrustedBefore string `yaml:"receipts_trusted_before,omitempty" json:"receipts_trusted_before,omitempty"`
}

type Project struct {
	ID     string            `yaml:"id" json:"id"`
	Number int               `yaml:"number" json:"number"`
	Title  string            `yaml:"title" json:"title"`
	Fields map[string]string `yaml:"fields" json:"fields"`
}

type Policy struct {
	LeaseTTLMinutes int `yaml:"lease_ttl_minutes" json:"lease_ttl_minutes"`
	ReconcileMins   int `yaml:"reconcile_minutes" json:"reconcile_minutes"`
}

func (c Config) Validate() error {
	if c.SchemaVersion != ConfigVersion {
		return fmt.Errorf("unsupported schema_version %d", c.SchemaVersion)
	}
	if strings.TrimSpace(c.Owner) == "" || strings.TrimSpace(c.Repository) == "" {
		return fmt.Errorf("owner and repository are required")
	}
	if c.DefaultBranch == "" {
		return fmt.Errorf("default_branch is required")
	}
	if c.WikiMode != "off" && c.WikiMode != "journal" {
		return fmt.Errorf("wiki_mode must be off or journal")
	}
	if len(c.Areas) == 0 {
		return fmt.Errorf("at least one area is required")
	}
	if len(c.AuthorizedActors) == 0 {
		return fmt.Errorf("at least one authorized actor is required")
	}
	actors := map[string]bool{}
	for _, actor := range c.AuthorizedActors {
		actor = strings.ToLower(strings.TrimSpace(actor))
		if actor == "" || actors[actor] {
			return fmt.Errorf("authorized actors must be nonempty and unique")
		}
		actors[actor] = true
	}
	if c.ReceiptsTrustedBefore != "" {
		if _, err := time.Parse(time.RFC3339, c.ReceiptsTrustedBefore); err != nil {
			return fmt.Errorf("receipts_trusted_before must be RFC3339")
		}
	}
	seen := map[string]bool{}
	for _, area := range c.Areas {
		if area == "" || seen[area] {
			return fmt.Errorf("areas must be nonempty and unique")
		}
		seen[area] = true
	}
	return nil
}
