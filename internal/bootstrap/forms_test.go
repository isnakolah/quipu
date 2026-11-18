package bootstrap

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/isnakolah/quipu/internal/model"
)

type formField struct {
	Type       string `yaml:"type"`
	ID         string `yaml:"id"`
	Attributes struct {
		Label string `yaml:"label"`
		Value string `yaml:"value"`
	} `yaml:"attributes"`
}

// renderForm builds the Issue body GitHub writes when a form is submitted:
// each field as a "### Label" heading followed by its answer, or by
// "_No response_" when it is empty. Markdown elements are not submitted.
func renderForm(t *testing.T, path string, answers map[string]string) (string, []string) {
	t.Helper()
	var form struct {
		Body []formField `yaml:"body"`
	}
	if err := yaml.Unmarshal([]byte(forms[path]), &form); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var body strings.Builder
	var labels []string
	for _, field := range form.Body {
		if field.Type == "markdown" {
			continue
		}
		labels = append(labels, field.Attributes.Label)
		answer, ok := answers[field.ID]
		if !ok {
			answer = field.Attributes.Value
		}
		if strings.TrimSpace(answer) == "" {
			answer = "_No response_"
		}
		body.WriteString("### " + field.Attributes.Label + "\n\n" + strings.TrimRight(answer, "\n") + "\n\n")
	}
	return body.String(), labels
}

func TestDecisionAndEpicFormsProduceBodiesThatPassTheirContracts(t *testing.T) {
	cases := []struct {
		path, kind string
		sections   []string
		answers    map[string]string
	}{
		{".github/ISSUE_TEMPLATE/decision.yml", "decision", model.RequiredDecisionSections, map[string]string{
			"question": "Should the Writer run on a schedule?",
			"context":  "Events are missed while the runner sleeps.",
			"drivers":  "- Receipts within five minutes",
			"options": "#### Schedule\n\nConsequences:\n- Good, because nothing waits long.\n\n" +
				"#### Events only\n\nConsequences:\n- Bad, because a sleeping runner misses events.",
			"recommendation": "Schedule, because missed receipts stall agents.",
			"approver":       "@octo",
			"blocks":         "- #12",
		}},
		{".github/ISSUE_TEMPLATE/epic.yml", "epic", model.RequiredEpicSections, map[string]string{
			"outcome":     "Quipu adopts private repositories.",
			"non_goals":   "- Hosting runners",
			"constraints": "- GitHub is the only record",
			"exit":        "- [ ] A private repository runs the full loop",
			"children":    "- Story: adopt audit",
			"approval":    "https://github.com/octo/r/issues/1#issuecomment-1",
		}},
	}
	for _, tc := range cases {
		body, labels := renderForm(t, tc.path, tc.answers)
		if strings.Join(labels, "|") != strings.Join(tc.sections, "|") {
			t.Errorf("%s fields %q, want the %s sections %q", tc.path, labels, model.ContractName(tc.kind), tc.sections)
		}
		if err := model.ValidateContract(tc.kind, body); err != nil {
			t.Errorf("%s: filled form fails %s: %v\n%s", tc.path, model.ContractName(tc.kind), err, body)
		}
		// Submitted with only the form's own prefilled values, it must not pass.
		blank, _ := renderForm(t, tc.path, map[string]string{})
		if err := model.ValidateContract(tc.kind, blank); err == nil {
			t.Errorf("%s: an unfilled form passes %s\n%s", tc.path, model.ContractName(tc.kind), blank)
		}
	}
}
