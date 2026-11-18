package model

// Likely duplicates. Before an Issue is filed or proposed, its title and Outcome
// are compared with each open Issue's, word by word, so an agent sees work that
// already exists before it adds the same work again. Only a warning: two
// Issues can share words and still be different work.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// DuplicateThreshold is the word overlap (Jaccard) at which two Issues look alike.
const DuplicateThreshold = 0.6

// OpenIssue is an open Issue as the duplicate check sees it.
type OpenIssue struct {
	Number int
	Title  string
	Body   string
}

// Similar is an open Issue that looks like the one being filed.
type Similar struct {
	Number int
	Title  string
	Score  float64
}

var wordPattern = regexp.MustCompile(`[\p{L}\p{N}]+`)

// stopWords carry no meaning on their own, so they never make two Issues alike.
var stopWords = setOfWords("a an and are as at be by for from has in into is it its of on or that the this to was were will with when what which who not no so than then there their them they we you your our can do does each every one")

func setOfWords(s string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.Fields(s) {
		set[w] = true
	}
	return set
}

// words is the set of meaningful lower-case words in s.
func words(s string) map[string]bool {
	set := map[string]bool{}
	for _, w := range wordPattern.FindAllString(strings.ToLower(s), -1) {
		if len(w) > 1 && !stopWords[w] {
			set[w] = true
		}
	}
	return set
}

// jaccard is the share of words two sets have in common, from 0 to 1.
func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	shared := 0
	for w := range a {
		if b[w] {
			shared++
		}
	}
	return float64(shared) / float64(len(a)+len(b)-shared)
}

// Outcome is the text of a body's "## Outcome" section, or "".
func Outcome(body string) string {
	s, ok := workSections(body)["outcome"]
	if !ok {
		return ""
	}
	return strings.TrimSpace(strings.Join(s.lines, "\n"))
}

// SimilarIssues lists the open Issues whose title or Outcome overlaps the new
// Issue's by at least DuplicateThreshold, most alike first.
func SimilarIssues(title, body string, open []OpenIssue) []Similar {
	newTitle, newOutcome := words(title), words(Outcome(body))
	var out []Similar
	for _, issue := range open {
		score := jaccard(newTitle, words(issue.Title))
		if outcome := jaccard(newOutcome, words(Outcome(issue.Body))); outcome > score {
			score = outcome
		}
		if score >= DuplicateThreshold {
			out = append(out, Similar{Number: issue.Number, Title: issue.Title, Score: score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// DuplicateWarnings words SimilarIssues as warnings.
func DuplicateWarnings(similar []Similar) []string {
	warnings := make([]string, 0, len(similar))
	for _, s := range similar {
		warnings = append(warnings, fmt.Sprintf("open #%d %q looks like the same work (%.0f%% of words shared); check it before adding this", s.Number, s.Title, s.Score*100))
	}
	return warnings
}
